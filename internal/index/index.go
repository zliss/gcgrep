// Package index implements an in-memory trigram index over file contents.
//
// Design: trigrams are lowercased so the index serves both case-sensitive and
// case-insensitive queries (candidates are a superset; the regex confirms).
// Updates never rewrite postings in place: the old file entry is tombstoned
// and a new entry appended, so posting lists stay sorted by construction.
// Compaction rebuilds everything once dead entries exceed 25%.
package index

import (
	"bytes"
	"regexp"
	"sync"
)

// MaxFileSize bounds indexed file size; larger files are skipped like binaries.
const MaxFileSize = 2 << 20

const (
	compactMinDead   = 64
	compactDeadRatio = 4 // compact when dead > live/4
)

type FileMeta struct {
	Path    string // slash-separated path relative to root
	Size    int64
	MtimeNS int64
}

type fileEntry struct {
	meta    FileMeta
	content []byte
	dead    bool
}

type Index struct {
	mu     sync.RWMutex
	root   string
	files  []*fileEntry
	byPath map[string]uint32
	tri    map[uint32][]uint32
	dead   int
}

func New(root string) *Index {
	return &Index{
		root:   root,
		byPath: make(map[string]uint32),
		tri:    make(map[uint32][]uint32),
	}
}

func (ix *Index) Root() string { return ix.root }

func lower(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + 32
	}
	return b
}

func trigramsOf(content []byte, seen map[uint32]struct{}) {
	for i := 0; i+3 <= len(content); i++ {
		k := uint32(lower(content[i]))<<16 | uint32(lower(content[i+1]))<<8 | uint32(lower(content[i+2]))
		seen[k] = struct{}{}
	}
}

// TrigramKeys computes the deduplicated trigram set of content. It takes
// no lock, so callers can parallelize this (the expensive part of Add)
// across files and only serialize the insertion.
func TrigramKeys(content []byte) []uint32 {
	seen := make(map[uint32]struct{}, len(content)/2)
	trigramsOf(content, seen)
	keys := make([]uint32, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	return keys
}

// Add inserts or replaces a file. content must not be mutated afterwards.
func (ix *Index) Add(meta FileMeta, content []byte) {
	ix.AddWithKeys(meta, content, TrigramKeys(content))
}

// AddWithKeys is Add with the trigram set precomputed via TrigramKeys.
func (ix *Index) AddWithKeys(meta FileMeta, content []byte, keys []uint32) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.removeLocked(meta.Path)
	id := uint32(len(ix.files))
	ix.files = append(ix.files, &fileEntry{meta: meta, content: content})
	ix.byPath[meta.Path] = id
	for _, k := range keys {
		ix.tri[k] = append(ix.tri[k], id)
	}
	ix.maybeCompactLocked()
}

func (ix *Index) Remove(path string) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.removeLocked(path)
	ix.maybeCompactLocked()
}

func (ix *Index) removeLocked(path string) {
	if id, ok := ix.byPath[path]; ok {
		ix.files[id].dead = true
		ix.dead++
		delete(ix.byPath, path)
	}
}

// RemovePrefix drops every indexed file under dir (used when a directory is deleted).
func (ix *Index) RemovePrefix(dir string) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	prefix := dir + "/"
	for p := range ix.byPath {
		if p == dir || len(p) > len(prefix) && p[:len(prefix)] == prefix {
			ix.removeLocked(p)
		}
	}
	ix.maybeCompactLocked()
}

func (ix *Index) maybeCompactLocked() {
	if ix.dead < compactMinDead || ix.dead*compactDeadRatio < len(ix.files)-ix.dead {
		return
	}
	files := make([]*fileEntry, 0, len(ix.files)-ix.dead)
	for _, f := range ix.files {
		if !f.dead {
			files = append(files, f)
		}
	}
	ix.files = files
	ix.dead = 0
	ix.byPath = make(map[string]uint32, len(files))
	ix.tri = make(map[uint32][]uint32)
	seen := make(map[uint32]struct{})
	for id, f := range files {
		ix.byPath[f.meta.Path] = uint32(id)
		clear(seen)
		trigramsOf(f.content, seen)
		for k := range seen {
			ix.tri[k] = append(ix.tri[k], uint32(id))
		}
	}
}

// Meta returns the stored metadata for path, if indexed.
func (ix *Index) Meta(path string) (FileMeta, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	if id, ok := ix.byPath[path]; ok {
		return ix.files[id].meta, true
	}
	return FileMeta{}, false
}

// Paths returns all live indexed paths.
func (ix *Index) Paths() []string {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	out := make([]string, 0, len(ix.byPath))
	for p := range ix.byPath {
		out = append(out, p)
	}
	return out
}

func (ix *Index) NumFiles() int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return len(ix.byPath)
}

// Snapshot returns metadata and content of all live files (for persistence).
func (ix *Index) Snapshot() ([]FileMeta, [][]byte) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	metas := make([]FileMeta, 0, len(ix.byPath))
	contents := make([][]byte, 0, len(ix.byPath))
	for _, f := range ix.files {
		if !f.dead {
			metas = append(metas, f.meta)
			contents = append(contents, f.content)
		}
	}
	return metas, contents
}

type Match struct {
	Path string
	Line int // 1-based
	Text string
}

type SearchOpts struct {
	Literal   string // required literal extracted from the pattern; "" = scan all files
	PathMatch func(path string) bool
	FilesOnly bool
	Limit     int // max matches; 0 = unlimited

	// PlainLiteral, when non-empty, replaces the regex with a direct
	// bytes.Index scan (FoldCase = ASCII case-insensitive). The regex is
	// still passed for interface uniformity but not executed.
	PlainLiteral string
	FoldCase     bool
}

type SearchResult struct {
	Matches    []Match
	FileCounts map[string]int // path -> match count (always filled)
	Truncated  bool
}

// Search runs re over candidate files chosen via the trigram index.
func (ix *Index) Search(re *regexp.Regexp, opts SearchOpts) SearchResult {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	res := SearchResult{FileCounts: make(map[string]int)}
	lit := []byte(opts.PlainLiteral)
	for _, id := range ix.candidatesLocked(opts.Literal) {
		f := ix.files[id]
		if f.dead {
			continue
		}
		if opts.PathMatch != nil && !opts.PathMatch(f.meta.Path) {
			continue
		}
		var locs [][]int
		if len(lit) > 0 {
			locs = literalFindAll(f.content, lit, opts.FoldCase)
		} else {
			locs = re.FindAllIndex(f.content, -1)
		}
		if len(locs) == 0 {
			continue
		}
		res.FileCounts[f.meta.Path] = len(locs)
		if opts.FilesOnly {
			continue
		}
		appendLineMatches(&res, f, locs, opts.Limit)
		if opts.Limit > 0 && len(res.Matches) >= opts.Limit {
			res.Truncated = true
			return res
		}
	}
	return res
}

func appendLineMatches(res *SearchResult, f *fileEntry, locs [][]int, limit int) {
	content := f.content
	lineNo, pos := 1, 0
	lastLine := -1
	for _, loc := range locs {
		for pos < loc[0] {
			nl := bytes.IndexByte(content[pos:loc[0]], '\n')
			if nl < 0 {
				pos = loc[0]
				break
			}
			pos += nl + 1
			lineNo++
		}
		if lineNo == lastLine {
			continue // one output line per source line, like grep
		}
		lastLine = lineNo
		start := pos
		if i := bytes.LastIndexByte(content[:loc[0]], '\n'); i >= 0 {
			start = i + 1
		} else {
			start = 0
		}
		end := loc[1]
		if i := bytes.IndexByte(content[end:], '\n'); i >= 0 {
			end += i
		} else {
			end = len(content)
		}
		res.Matches = append(res.Matches, Match{Path: f.meta.Path, Line: lineNo, Text: string(content[start:end])})
		if limit > 0 && len(res.Matches) >= limit {
			return
		}
	}
}

// candidatesLocked returns file IDs that may contain the literal, via
// intersection of its trigram posting lists. Empty literal means all files.
func (ix *Index) candidatesLocked(lit string) []uint32 {
	if len(lit) < 3 {
		all := make([]uint32, 0, len(ix.byPath))
		for _, id := range ix.byPath {
			all = append(all, id)
		}
		return all
	}
	b := []byte(lit)
	seen := make(map[uint32]struct{})
	trigramsOf(b, seen)
	var lists [][]uint32
	for k := range seen {
		l, ok := ix.tri[k]
		if !ok {
			return nil
		}
		lists = append(lists, l)
	}
	// intersect, starting from the shortest list
	shortest := 0
	for i, l := range lists {
		if len(l) < len(lists[shortest]) {
			shortest = i
		}
	}
	out := lists[shortest]
	for i, l := range lists {
		if i == shortest {
			continue
		}
		out = intersectSorted(out, l)
		if len(out) == 0 {
			return nil
		}
	}
	return out
}

func intersectSorted(a, b []uint32) []uint32 {
	out := a[:0:0]
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] < b[j]:
			i++
		case a[i] > b[j]:
			j++
		default:
			out = append(out, a[i])
			i++
			j++
		}
	}
	return out
}
