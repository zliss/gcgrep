// engine.go implements the disk-backed search engine: multiple immutable
// shards + a dirty list + background rebuild. It replaces the in-memory
// index.Index for large roots while keeping symbol tables in memory.
package shard

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zliss/gcgrep/internal/index"
	"github.com/zliss/gcgrep/internal/symbol"
)

// MaxShardFiles is the target file count per shard.
const MaxShardFiles = 10000

// MaxShardBytes is the target source-size per shard.
const MaxShardBytes = 256 << 20

// DiskEngine holds shards, a dirty list, and an in-memory symbol table.
type DiskEngine struct {
	root     string
	shardDir string
	workers  int

	mu     sync.RWMutex
	shards []*Shard                // immutable; replaced atomically on rebuild
	seqNo  int                     // next shard filename sequence number
	syms   map[string][]symbol.Def // path → defs (in-memory, ~1-3% of source)
	// pathToShard maps rel path → shard index for quick dirty-file lookups
	pathToShard map[string]int
	totalFiles  atomic.Int64
	totalBytes  atomic.Int64

	dirtyMu sync.Mutex
	dirty   map[string]dirtyEntry // rel → entry
	closed  chan struct{}

	rebuildInterval time.Duration
}

type dirtyEntry struct {
	deleted bool
}

// NewDiskEngine creates a disk engine rooted at root, storing shards under
// shardDir (must exist). rebuildInterval controls the background rebuild
// cycle (0 = no auto-rebuild).
func NewDiskEngine(root, shardDir string, workers int, rebuildInterval time.Duration) *DiskEngine {
	e := &DiskEngine{
		root:            root,
		shardDir:        shardDir,
		workers:         workers,
		syms:            make(map[string][]symbol.Def),
		pathToShard:     make(map[string]int),
		dirty:           make(map[string]dirtyEntry),
		closed:          make(chan struct{}),
		rebuildInterval: rebuildInterval,
	}
	if rebuildInterval > 0 {
		go e.rebuildLoop()
	}
	return e
}

func (e *DiskEngine) Close() {
	select {
	case <-e.closed:
	default:
		close(e.closed)
	}
}

// ---- Build / Reconcile ----

// BuildFile is a file discovered by the walker, passed to FullBuild/Reconcile.
type BuildFile struct {
	Rel     string
	Size    int64
	MtimeNS int64
}

// FullBuild creates shards for all files. Called on first index of a root.
func (e *DiskEngine) FullBuild(files []BuildFile, progress func(indexed int)) error {
	// group files into shard-sized batches
	batches := groupFiles(files)
	shards := make([]*Shard, len(batches))
	pathToShard := make(map[string]int, len(files))
	syms := make(map[string][]symbol.Def, len(files))
	var totalBytes int64

	for bi, batch := range batches {
		b := NewBuilder()
		for _, f := range batch {
			abs := filepath.Join(e.root, filepath.FromSlash(f.Rel))
			content, err := os.ReadFile(abs)
			if err != nil {
				continue
			}
			if decoded, ok := decodeUTF16(content); ok {
				content = decoded
			}
			if isBinary(content) {
				continue
			}
			b.Add(f.Rel, f.Size, f.MtimeNS, content)
			defs := symbol.Extract(f.Rel, content)
			if len(defs) > 0 {
				syms[f.Rel] = defs
			}
			totalBytes += f.Size
			if progress != nil {
				progress(1)
			}
		}
		path := e.shardPath(bi)
		if err := b.Write(path); err != nil {
			return fmt.Errorf("write shard %d: %w", bi, err)
		}
		s, err := Load(path)
		if err != nil {
			return fmt.Errorf("load shard %d: %w", bi, err)
		}
		shards[bi] = s
		for j := 0; j < s.NumFiles(); j++ {
			pathToShard[s.File(j).Path] = bi
		}
	}

	e.mu.Lock()
	e.shards = shards
	e.seqNo = len(shards)
	e.syms = syms
	e.pathToShard = pathToShard
	e.totalFiles.Store(int64(len(pathToShard)))
	e.totalBytes.Store(totalBytes)
	e.mu.Unlock()
	return nil
}

func groupFiles(files []BuildFile) [][]BuildFile {
	var batches [][]BuildFile
	var cur []BuildFile
	var curBytes int64
	for _, f := range files {
		cur = append(cur, f)
		curBytes += f.Size
		if len(cur) >= MaxShardFiles || curBytes >= MaxShardBytes {
			batches = append(batches, cur)
			cur = nil
			curBytes = 0
		}
	}
	if len(cur) > 0 {
		batches = append(batches, cur)
	}
	return batches
}

func (e *DiskEngine) shardPath(seq int) string {
	return filepath.Join(e.shardDir, fmt.Sprintf("shard-%04d.idx", seq))
}

// Reconcile loads existing shards and verifies file metadata against disk.
// Stale files are added to the dirty list for the next rebuild cycle.
func (e *DiskEngine) Reconcile(diskFiles []BuildFile) {
	// load any existing shards
	entries, _ := os.ReadDir(e.shardDir)
	var shards []*Shard
	for _, ent := range entries {
		if !strings.HasSuffix(ent.Name(), ".idx") {
			continue
		}
		s, err := Load(filepath.Join(e.shardDir, ent.Name()))
		if err != nil {
			log.Printf("shard load %s: %v (will rebuild)", ent.Name(), err)
			continue
		}
		shards = append(shards, s)
	}

	onDisk := make(map[string]BuildFile, len(diskFiles))
	for _, f := range diskFiles {
		onDisk[f.Rel] = f
	}

	pathToShard := make(map[string]int)
	syms := make(map[string][]symbol.Def)
	var totalBytes int64

	for si, s := range shards {
		for i := 0; i < s.NumFiles(); i++ {
			fm := s.File(i)
			pathToShard[fm.Path] = si
			totalBytes += fm.Size
			df, ok := onDisk[fm.Path]
			if !ok || df.Size != fm.Size || df.MtimeNS != fm.MtimeNS {
				e.dirtyMu.Lock()
				e.dirty[fm.Path] = dirtyEntry{deleted: !ok}
				e.dirtyMu.Unlock()
			}
			// rebuild symbol table from disk
			if ok {
				abs := filepath.Join(e.root, filepath.FromSlash(fm.Path))
				content, err := os.ReadFile(abs)
				if err == nil {
					if decoded, okd := decodeUTF16(content); okd {
						content = decoded
					}
					if !isBinary(content) {
						if defs := symbol.Extract(fm.Path, content); len(defs) > 0 {
							syms[fm.Path] = defs
						}
					}
				}
			}
		}
	}

	// files on disk but not in any shard → dirty (new files)
	for _, f := range diskFiles {
		if _, ok := pathToShard[f.Rel]; !ok {
			e.dirtyMu.Lock()
			e.dirty[f.Rel] = dirtyEntry{}
			e.dirtyMu.Unlock()
		}
	}

	e.mu.Lock()
	e.shards = shards
	e.seqNo = len(shards)
	e.syms = syms
	e.pathToShard = pathToShard
	e.totalFiles.Store(int64(len(onDisk)))
	e.totalBytes.Store(totalBytes)
	e.mu.Unlock()
}

// ---- Dirty list ----

// MarkDirty flags a file for re-scanning on next query and rebuild.
func (e *DiskEngine) MarkDirty(rel string) {
	e.dirtyMu.Lock()
	e.dirty[rel] = dirtyEntry{}
	e.dirtyMu.Unlock()
	// update symbol table
	abs := filepath.Join(e.root, filepath.FromSlash(rel))
	content, err := os.ReadFile(abs)
	if err != nil {
		e.mu.Lock()
		delete(e.syms, rel)
		e.mu.Unlock()
		return
	}
	if decoded, ok := decodeUTF16(content); ok {
		content = decoded
	}
	if !isBinary(content) {
		defs := symbol.Extract(rel, content)
		e.mu.Lock()
		if len(defs) > 0 {
			e.syms[rel] = defs
		} else {
			delete(e.syms, rel)
		}
		e.mu.Unlock()
	}
}

// MarkDeleted flags a file as deleted.
func (e *DiskEngine) MarkDeleted(rel string) {
	e.dirtyMu.Lock()
	e.dirty[rel] = dirtyEntry{deleted: true}
	e.dirtyMu.Unlock()
	e.mu.Lock()
	delete(e.syms, rel)
	e.mu.Unlock()
}

// MarkDeletedPrefix flags all files under dir as deleted.
func (e *DiskEngine) MarkDeletedPrefix(dir string) {
	prefix := dir + "/"
	e.mu.RLock()
	var toDelete []string
	for p := range e.pathToShard {
		if p == dir || strings.HasPrefix(p, prefix) {
			toDelete = append(toDelete, p)
		}
	}
	e.mu.RUnlock()
	e.dirtyMu.Lock()
	for _, p := range toDelete {
		e.dirty[p] = dirtyEntry{deleted: true}
	}
	e.dirtyMu.Unlock()
	e.mu.Lock()
	for _, p := range toDelete {
		delete(e.syms, p)
	}
	e.mu.Unlock()
}

func (e *DiskEngine) DirtyCount() int {
	e.dirtyMu.Lock()
	defer e.dirtyMu.Unlock()
	return len(e.dirty)
}

// ---- Search ----

// Search runs a text query across all shards + dirty files.
func (e *DiskEngine) Search(re *regexp.Regexp, opts index.SearchOpts) index.SearchResult {
	e.mu.RLock()
	shards := e.shards
	e.mu.RUnlock()

	e.dirtyMu.Lock()
	dirtySnap := make(map[string]dirtyEntry, len(e.dirty))
	for k, v := range e.dirty {
		dirtySnap[k] = v
	}
	e.dirtyMu.Unlock()

	res := index.SearchResult{FileCounts: make(map[string]int)}
	mat, _ := index.MatcherFor(re.String(), false, false)
	// re-derive the matcher from opts to respect PlainLiteral/FoldCase
	if opts.PlainLiteral != "" {
		mat.PlainLit = opts.PlainLiteral
		mat.Fold = opts.FoldCase
		mat.Re = re
	}

	// 1. search shards
	for _, s := range shards {
		candidates := s.Candidates(opts.Literal)
		for _, id := range candidates {
			fm := s.File(int(id))
			if _, isDirty := dirtySnap[fm.Path]; isDirty {
				continue // will scan from disk below
			}
			if opts.MaxFileSize > 0 && fm.Size > opts.MaxFileSize {
				continue
			}
			if opts.PathMatch != nil && !opts.PathMatch(fm.Path) {
				continue
			}
			// verify by reading the original file
			matches := e.verifyFile(fm.Path, fm, mat, opts)
			if len(matches) > 0 {
				res.FileCounts[fm.Path] = len(matches)
				if !opts.FilesOnly {
					res.Matches = append(res.Matches, matches...)
				}
			}
			if opts.Limit > 0 && len(res.Matches) >= opts.Limit {
				res.Truncated = true
				return res
			}
		}
	}

	// 2. scan dirty files from disk
	var dirtyPaths []string
	for p, de := range dirtySnap {
		if de.deleted {
			continue
		}
		dirtyPaths = append(dirtyPaths, p)
	}
	sort.Strings(dirtyPaths)
	for _, rel := range dirtyPaths {
		if opts.PathMatch != nil && !opts.PathMatch(rel) {
			continue
		}
		abs := filepath.Join(e.root, filepath.FromSlash(rel))
		fi, err := os.Stat(abs)
		if err != nil {
			continue
		}
		if opts.MaxFileSize > 0 && fi.Size() > opts.MaxFileSize {
			continue
		}
		fm := FileMeta{Path: rel, Size: fi.Size(), MtimeNS: fi.ModTime().UnixNano()}
		matches := e.verifyFile(rel, fm, mat, opts)
		if len(matches) > 0 {
			res.FileCounts[rel] = len(matches)
			if !opts.FilesOnly {
				res.Matches = append(res.Matches, matches...)
			}
		}
		if opts.Limit > 0 && len(res.Matches) >= opts.Limit {
			res.Truncated = true
			return res
		}
	}

	return res
}

// verifyFile reads a file from disk and scans it line by line.
func (e *DiskEngine) verifyFile(rel string, fm FileMeta, mat index.Matcher, opts index.SearchOpts) []index.Match {
	abs := filepath.Join(e.root, filepath.FromSlash(rel))
	f, err := os.Open(abs)
	if err != nil {
		return nil
	}
	defer f.Close()
	// stat check: if mtime/size changed, the file is stale (race with
	// rebuild or very recent write); scan it anyway — correctness is
	// maintained by the dirty list, and showing slightly stale results
	// is harmless until the rebuild cycle catches up.

	r := bufio.NewReaderSize(f, 1<<20)
	head, _ := r.Peek(8192)
	if bytes.IndexByte(head, 0) >= 0 {
		return nil // binary
	}

	var matches []index.Match
	lineNo := 0
	for {
		line, rerr := r.ReadBytes('\n')
		if len(line) > 0 {
			lineNo++
			trimmed := line
			if n := len(trimmed); n > 0 && trimmed[n-1] == '\n' {
				trimmed = trimmed[:n-1]
			}
			if loc := mat.FindFirst(trimmed); loc != nil {
				m := index.Match{Path: rel, Line: lineNo, Col: loc[0], LineLen: len(trimmed)}
				if opts.MaxColumns <= 0 || len(trimmed) <= opts.MaxColumns {
					m.Text = string(trimmed)
				}
				matches = append(matches, m)
				if opts.FilesOnly {
					return matches
				}
				if opts.Limit > 0 && len(matches) >= opts.Limit {
					return matches
				}
			}
		}
		if rerr == io.EOF {
			return matches
		}
		if rerr != nil {
			return matches
		}
	}
}

// ---- Symbol methods (in-memory) ----

func (e *DiskEngine) Defs(name string, fold bool, pathMatch func(string) bool) []index.DefHit {
	e.mu.RLock()
	defer e.mu.RUnlock()
	want := name
	if fold {
		want = strings.ToLower(name)
	}
	var hits []index.DefHit
	for path, defs := range e.syms {
		if pathMatch != nil && !pathMatch(path) {
			continue
		}
		for _, d := range defs {
			got := d.Name
			if fold {
				got = strings.ToLower(got)
			}
			if got == want {
				text := e.readLine(path, d.Line)
				hits = append(hits, index.DefHit{Path: path, Def: d, Text: text})
			}
		}
	}
	return hits
}

func (e *DiskEngine) FileDefs(path string) ([]index.DefHit, bool) {
	e.mu.RLock()
	defs, ok := e.syms[path]
	e.mu.RUnlock()
	if !ok {
		return nil, false
	}
	hits := make([]index.DefHit, 0, len(defs))
	for _, d := range defs {
		hits = append(hits, index.DefHit{Path: path, Def: d, Text: e.readLine(path, d.Line)})
	}
	return hits, true
}

func (e *DiskEngine) FilesContaining(lit string, pathMatch func(string) bool) []index.FileContent {
	e.mu.RLock()
	shards := e.shards
	e.mu.RUnlock()

	needle := []byte(lit)
	var out []index.FileContent
	seen := make(map[string]bool)

	for _, s := range shards {
		candidates := s.Candidates(strings.ToLower(lit))
		for _, id := range candidates {
			fm := s.File(int(id))
			if seen[fm.Path] || (pathMatch != nil && !pathMatch(fm.Path)) {
				continue
			}
			seen[fm.Path] = true
			content, err := os.ReadFile(filepath.Join(e.root, filepath.FromSlash(fm.Path)))
			if err != nil {
				continue
			}
			if bytes.Contains(content, needle) {
				out = append(out, index.FileContent{Path: fm.Path, Content: content})
			}
		}
	}

	// also check dirty (non-deleted) files
	e.dirtyMu.Lock()
	var dirtyPaths []string
	for p, de := range e.dirty {
		if !de.deleted && !seen[p] {
			dirtyPaths = append(dirtyPaths, p)
		}
	}
	e.dirtyMu.Unlock()
	for _, p := range dirtyPaths {
		if pathMatch != nil && !pathMatch(p) {
			continue
		}
		content, err := os.ReadFile(filepath.Join(e.root, filepath.FromSlash(p)))
		if err != nil {
			continue
		}
		if bytes.Contains(content, needle) {
			out = append(out, index.FileContent{Path: p, Content: content})
		}
	}
	return out
}

func (e *DiskEngine) readLine(rel string, lineNo int) string {
	abs := filepath.Join(e.root, filepath.FromSlash(rel))
	content, err := os.ReadFile(abs)
	if err != nil {
		return ""
	}
	return index.LineText(content, lineNo)
}

// ---- Status ----

func (e *DiskEngine) NumFiles() int     { return int(e.totalFiles.Load()) }
func (e *DiskEngine) TotalBytes() int64 { return e.totalBytes.Load() }
func (e *DiskEngine) NumShards() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return len(e.shards)
}

// ---- Meta for reconcile ----

// Meta returns file metadata from the shards (for reconcile comparisons).
func (e *DiskEngine) Meta(path string) (FileMeta, bool) {
	e.mu.RLock()
	si, ok := e.pathToShard[path]
	if !ok {
		e.mu.RUnlock()
		return FileMeta{}, false
	}
	s := e.shards[si]
	e.mu.RUnlock()
	for i := 0; i < s.NumFiles(); i++ {
		fm := s.File(i)
		if fm.Path == path {
			return fm, true
		}
	}
	return FileMeta{}, false
}

// Paths returns all paths known to the shards.
func (e *DiskEngine) Paths() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]string, 0, len(e.pathToShard))
	for p := range e.pathToShard {
		out = append(out, p)
	}
	return out
}

// ---- Background rebuild ----

func (e *DiskEngine) rebuildLoop() {
	ticker := time.NewTicker(e.rebuildInterval)
	defer ticker.Stop()
	for {
		select {
		case <-e.closed:
			return
		case <-ticker.C:
			e.rebuild()
		}
	}
}

func (e *DiskEngine) rebuild() {
	e.dirtyMu.Lock()
	if len(e.dirty) == 0 {
		e.dirtyMu.Unlock()
		return
	}
	snap := make(map[string]dirtyEntry, len(e.dirty))
	for k, v := range e.dirty {
		snap[k] = v
	}
	e.dirtyMu.Unlock()

	e.mu.RLock()
	oldShards := e.shards
	e.mu.RUnlock()

	// determine which shards are affected
	affected := make(map[int]bool)
	e.mu.RLock()
	for p := range snap {
		if si, ok := e.pathToShard[p]; ok {
			affected[si] = true
		}
	}
	e.mu.RUnlock()

	// files in dirty but not in any shard → new files, build a new shard
	var newFiles []BuildFile
	for p, de := range snap {
		e.mu.RLock()
		_, inShard := e.pathToShard[p]
		e.mu.RUnlock()
		if !inShard && !de.deleted {
			abs := filepath.Join(e.root, filepath.FromSlash(p))
			fi, err := os.Stat(abs)
			if err == nil {
				newFiles = append(newFiles, BuildFile{Rel: p, Size: fi.Size(), MtimeNS: fi.ModTime().UnixNano()})
			}
		}
	}

	// rebuild affected shards
	e.mu.Lock()
	seqNo := e.seqNo
	e.mu.Unlock()

	newShards := make([]*Shard, len(oldShards))
	copy(newShards, oldShards)
	newPathToShard := make(map[string]int)
	var totalBytes int64
	newSyms := make(map[string][]symbol.Def)

	// copy unchanged shard entries
	e.mu.RLock()
	for p, d := range e.syms {
		newSyms[p] = d
	}
	e.mu.RUnlock()

	for si, s := range oldShards {
		if !affected[si] {
			for i := 0; i < s.NumFiles(); i++ {
				fm := s.File(i)
				newPathToShard[fm.Path] = si
				totalBytes += fm.Size
			}
			continue
		}
		// rebuild this shard: keep non-dirty files + re-read dirty ones
		b := NewBuilder()
		for i := 0; i < s.NumFiles(); i++ {
			fm := s.File(i)
			if de, isDirty := snap[fm.Path]; isDirty {
				if de.deleted {
					delete(newSyms, fm.Path)
					continue
				}
				// dirty non-deleted: re-read from disk
				abs := filepath.Join(e.root, filepath.FromSlash(fm.Path))
				content, err := os.ReadFile(abs)
				if err != nil {
					delete(newSyms, fm.Path)
					continue
				}
				fi, ferr := os.Stat(abs)
				if ferr != nil {
					continue
				}
				if decoded, ok := decodeUTF16(content); ok {
					content = decoded
				}
				if isBinary(content) {
					delete(newSyms, fm.Path)
					continue
				}
				b.Add(fm.Path, fi.Size(), fi.ModTime().UnixNano(), content)
				if defs := symbol.Extract(fm.Path, content); len(defs) > 0 {
					newSyms[fm.Path] = defs
				} else {
					delete(newSyms, fm.Path)
				}
				totalBytes += fi.Size()
			} else {
				// unchanged: re-read to get trigrams
				abs := filepath.Join(e.root, filepath.FromSlash(fm.Path))
				content, err := os.ReadFile(abs)
				if err != nil {
					continue
				}
				if decoded, ok := decodeUTF16(content); ok {
					content = decoded
				}
				b.Add(fm.Path, fm.Size, fm.MtimeNS, content)
				totalBytes += fm.Size
			}
		}
		if b.NumFiles() == 0 {
			// shard became empty; nil it out
			newShards[si] = nil
			continue
		}
		path := e.shardPath(seqNo)
		seqNo++
		if err := b.Write(path); err != nil {
			log.Printf("rebuild shard: %v", err)
			// keep old shard; dirty list will retry next cycle
			for i := 0; i < s.NumFiles(); i++ {
				fm := s.File(i)
				newPathToShard[fm.Path] = si
				totalBytes += fm.Size
			}
			continue
		}
		ns, err := Load(path)
		if err != nil {
			log.Printf("load rebuilt shard: %v", err)
			continue
		}
		newShards[si] = ns
		for i := 0; i < ns.NumFiles(); i++ {
			newPathToShard[ns.File(i).Path] = si
		}
	}

	// build shard for new files
	if len(newFiles) > 0 {
		b := NewBuilder()
		for _, f := range newFiles {
			abs := filepath.Join(e.root, filepath.FromSlash(f.Rel))
			content, err := os.ReadFile(abs)
			if err != nil {
				continue
			}
			if decoded, ok := decodeUTF16(content); ok {
				content = decoded
			}
			if isBinary(content) {
				continue
			}
			b.Add(f.Rel, f.Size, f.MtimeNS, content)
			if defs := symbol.Extract(f.Rel, content); len(defs) > 0 {
				newSyms[f.Rel] = defs
			}
			totalBytes += f.Size
		}
		if b.NumFiles() > 0 {
			path := e.shardPath(seqNo)
			seqNo++
			if err := b.Write(path); err == nil {
				ns, lerr := Load(path)
				if lerr == nil {
					si := len(newShards)
					newShards = append(newShards, ns)
					for i := 0; i < ns.NumFiles(); i++ {
						newPathToShard[ns.File(i).Path] = si
					}
				}
			}
		}
	}

	// compact out nil shards
	var compacted []*Shard
	remap := make(map[int]int)
	for i, s := range newShards {
		if s != nil {
			remap[i] = len(compacted)
			compacted = append(compacted, s)
		}
	}
	finalPTS := make(map[string]int, len(newPathToShard))
	for p, si := range newPathToShard {
		if nsi, ok := remap[si]; ok {
			finalPTS[p] = nsi
		}
	}

	e.mu.Lock()
	e.shards = compacted
	e.seqNo = seqNo
	e.syms = newSyms
	e.pathToShard = finalPTS
	e.totalFiles.Store(int64(len(finalPTS)))
	e.totalBytes.Store(totalBytes)
	e.mu.Unlock()

	// clear processed dirty entries
	e.dirtyMu.Lock()
	for p := range snap {
		if cur, ok := e.dirty[p]; ok && cur == snap[p] {
			delete(e.dirty, p)
		}
	}
	e.dirtyMu.Unlock()

	log.Printf("shard rebuild: %d shards, %d files", len(compacted), len(finalPTS))
}

// ---- helpers (shared with daemon package, duplicated to avoid import cycle) ----

func isBinary(content []byte) bool {
	n := len(content)
	if n > 8192 {
		n = 8192
	}
	return bytes.IndexByte(content[:n], 0) >= 0
}

func decodeUTF16(b []byte) ([]byte, bool) {
	var be bool
	switch {
	case len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE:
		be = false
	case len(b) >= 2 && b[0] == 0xFE && b[1] == 0xFF:
		be = true
	default:
		return nil, false
	}
	b = b[2:]
	u16 := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		if be {
			u16 = append(u16, uint16(b[i])<<8|uint16(b[i+1]))
		} else {
			u16 = append(u16, uint16(b[i+1])<<8|uint16(b[i]))
		}
	}
	runes := make([]rune, len(u16))
	for i, c := range u16 {
		runes[i] = rune(c)
	}
	out := []byte(string(runes))
	return out, true
}
