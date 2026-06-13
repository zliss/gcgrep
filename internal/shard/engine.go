// engine.go implements the disk-backed search engine: ordered immutable
// shards + a dirty list + background rebuild. Shards are mmap'd only
// during queries, so idle RSS is independent of repository size.
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
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zliss/gcgrep/internal/index"
	"github.com/zliss/gcgrep/internal/symbol"
)

var readerPool = sync.Pool{
	New: func() any { return bufio.NewReaderSize(nil, 1<<20) },
}

// DiskEngineConfig holds configurable parameters for the disk engine.
type DiskEngineConfig struct {
	ShardSize        ShardSizeConfig
	RebuildThreshold int
	SearchWorkers    int
	InlineKB         int // files ≤ this size have content stored in shard (Windows only, 0 = disabled)
}

// DiskEngine holds ordered shards, a dirty list, and no in-memory symbol
// table. Symbol queries read from disk on demand.
type DiskEngine struct {
	root     string
	shardDir string
	workers  int
	cfg      DiskEngineConfig

	// snapshot holds the current set of shards. Replaced atomically on rebuild.
	snapshot atomic.Pointer[engineSnapshot]

	totalFiles atomic.Int64
	totalBytes atomic.Int64

	dirtyMu sync.Mutex
	dirty   map[string]dirtyEntry

	// pendingDelete tracks shard files that should be removed but may
	// still be mmap'd by in-flight queries (Windows file locking).
	pendingMu sync.Mutex
	pending   []string

	closed          chan struct{}
	rebuildInterval time.Duration
}

// engineSnapshot holds an immutable view of the shard set. No per-file
// path index — shard lookup uses binary search on ordered [pathMin, pathMax].
type engineSnapshot struct {
	shards []*Shard
	seqNo  int
}

type dirtyEntry struct {
	deleted bool
}

// shardForPath returns the shard index that may contain path, using binary
// search on ordered, non-overlapping shard ranges. Returns -1 if no shard
// covers the path.
func (snap *engineSnapshot) shardForPath(path string) int {
	n := len(snap.shards)
	// binary search: find last shard whose pathMin <= path
	i := sort.Search(n, func(i int) bool {
		return snap.shards[i].pathMin > path
	}) - 1
	if i < 0 {
		return -1
	}
	s := snap.shards[i]
	if path >= s.pathMin && path <= s.pathMax {
		return i
	}
	return -1
}

// shardsForPrefix returns indices of all shards that may contain paths
// with the given prefix.
func (snap *engineSnapshot) shardsForPrefix(prefix string) []int {
	var out []int
	for i, s := range snap.shards {
		// shard overlaps prefix if pathMax >= prefix AND pathMin starts
		// with prefix OR pathMin < prefix+maxchar
		if s.pathMax < prefix {
			continue
		}
		if strings.HasPrefix(s.pathMin, prefix) || s.pathMin <= prefix {
			out = append(out, i)
			continue
		}
		// pathMin > prefix and doesn't start with prefix: no overlap
		break
	}
	return out
}

func NewDiskEngine(root, shardDir string, workers int, rebuildInterval time.Duration, cfg DiskEngineConfig) *DiskEngine {
	e := &DiskEngine{
		root:            root,
		shardDir:        shardDir,
		workers:         workers,
		cfg:             cfg,
		dirty:           make(map[string]dirtyEntry),
		closed:          make(chan struct{}),
		rebuildInterval: rebuildInterval,
	}
	e.snapshot.Store(&engineSnapshot{})
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

// FullBuild creates shards for all files. Called on first index of a root.
func (e *DiskEngine) FullBuild(files []BuildFile, progress func(indexed int)) error {
	batches := GroupByDirBoundary(files, e.cfg.ShardSize)
	shards := make([]*Shard, 0, len(batches))
	var totalFiles int
	var totalBytes int64

	seqNo := 0
	for _, batch := range batches {
		b := NewBuilder(e.cfg.InlineKB)
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
			defs := symbol.Extract(f.Rel, content)
			b.AddWithDefs(f.Rel, f.Size, f.MtimeNS, content, defs)
			totalBytes += f.Size
			totalFiles++
			if progress != nil {
				progress(1)
			}
		}
		if b.NumFiles() == 0 {
			continue
		}
		path := e.shardPath(seqNo)
		seqNo++
		if err := b.Write(path); err != nil {
			return fmt.Errorf("write shard: %w", err)
		}
		s, err := Open(path)
		if err != nil {
			return fmt.Errorf("open shard: %w", err)
		}
		shards = append(shards, s)
	}

	snap := &engineSnapshot{shards: shards, seqNo: seqNo}
	e.snapshot.Store(snap)
	e.totalFiles.Store(int64(totalFiles))
	e.totalBytes.Store(totalBytes)

	m := ManifestFromShards(shards, seqNo)
	if err := WriteManifest(e.shardDir, m); err != nil {
		log.Printf("write manifest: %v", err)
	}
	return nil
}

// Reconcile loads existing shards from manifest and verifies file metadata
// against disk. Stale files are added to the dirty list for rebuild.
func (e *DiskEngine) Reconcile(diskFiles []BuildFile) {
	manifest, err := ReadManifest(e.shardDir)
	if err != nil {
		log.Printf("read manifest: %v", err)
	}

	var shards []*Shard
	seqNo := 0
	if manifest != nil {
		seqNo = manifest.SeqNo
		for _, ms := range manifest.Shards {
			path := filepath.Join(e.shardDir, ms.File)
			s, err := Open(path)
			if err != nil {
				log.Printf("shard open %s: %v (will rebuild)", ms.File, err)
				continue
			}
			shards = append(shards, s)
		}
	}
	CleanOrphans(e.shardDir, manifest)

	onDisk := make(map[string]BuildFile, len(diskFiles))
	for _, f := range diskFiles {
		onDisk[f.Rel] = f
	}

	// check each shard's files against disk
	inShard := make(map[string]bool, len(diskFiles))
	var totalBytes int64

	for _, s := range shards {
		if err := s.Mmap(); err != nil {
			log.Printf("mmap shard for reconcile: %v", err)
			continue
		}
		for i := 0; i < s.NumFiles(); i++ {
			fm := s.File(i)
			inShard[fm.Path] = true
			totalBytes += fm.Size
			df, ok := onDisk[fm.Path]
			if !ok || df.Size != fm.Size || df.MtimeNS != fm.MtimeNS {
				e.dirtyMu.Lock()
				e.dirty[fm.Path] = dirtyEntry{deleted: !ok}
				e.dirtyMu.Unlock()
			}
		}
		s.Munmap()
	}

	// files on disk but not in any shard → dirty (new files)
	for _, f := range diskFiles {
		if !inShard[f.Rel] {
			e.dirtyMu.Lock()
			e.dirty[f.Rel] = dirtyEntry{}
			e.dirtyMu.Unlock()
		}
	}

	snap := &engineSnapshot{shards: shards, seqNo: seqNo}
	e.snapshot.Store(snap)
	e.totalFiles.Store(int64(len(onDisk)))
	e.totalBytes.Store(totalBytes)
}

func (e *DiskEngine) shardPath(seq int) string {
	return filepath.Join(e.shardDir, fmt.Sprintf("shard-%04d.idx", seq))
}

// ---- Dirty list ----

func (e *DiskEngine) MarkDirty(rel string) {
	e.dirtyMu.Lock()
	e.dirty[rel] = dirtyEntry{}
	e.dirtyMu.Unlock()
}

func (e *DiskEngine) MarkDeleted(rel string) {
	e.dirtyMu.Lock()
	e.dirty[rel] = dirtyEntry{deleted: true}
	e.dirtyMu.Unlock()
}

func (e *DiskEngine) MarkDeletedPrefix(dir string) {
	prefix := dir + "/"
	snap := e.snapshot.Load()
	indices := snap.shardsForPrefix(prefix)
	var toDelete []string
	for _, si := range indices {
		s := snap.shards[si]
		if err := s.Mmap(); err != nil {
			continue
		}
		for i := 0; i < s.NumFiles(); i++ {
			p := s.File(i).Path
			if p == dir || strings.HasPrefix(p, prefix) {
				toDelete = append(toDelete, p)
			}
		}
		s.Munmap()
	}
	if len(toDelete) == 0 {
		return
	}
	e.dirtyMu.Lock()
	for _, p := range toDelete {
		e.dirty[p] = dirtyEntry{deleted: true}
	}
	e.dirtyMu.Unlock()
}

func (e *DiskEngine) DirtyCount() int {
	e.dirtyMu.Lock()
	defer e.dirtyMu.Unlock()
	return len(e.dirty)
}

// ---- Search ----

func (e *DiskEngine) Search(re *regexp.Regexp, opts index.SearchOpts) index.SearchResult {
	snap := e.snapshot.Load()

	e.dirtyMu.Lock()
	dirtySnap := make(map[string]dirtyEntry, len(e.dirty))
	for k, v := range e.dirty {
		dirtySnap[k] = v
	}
	e.dirtyMu.Unlock()

	res := index.SearchResult{FileCounts: make(map[string]int)}
	mat, _ := index.MatcherFor(re.String(), false, false)
	if opts.PlainLiteral != "" {
		mat.PlainLit = opts.PlainLiteral
		mat.Fold = opts.FoldCase
		mat.Re = re
	}

	// 1. search shards in parallel (2 goroutines overlap I/O waits).
	//    Each shard writes to its own slot — no lock, results stay ordered.
	type shardResult struct {
		matches    []index.Match
		fileCounts map[string]int
	}
	nShards := len(snap.shards)
	results := make([]shardResult, nShards)
	var truncated atomic.Bool

	type shardJob struct {
		idx   int
		shard *Shard
	}
	jobs := make(chan shardJob, nShards)
	for i, s := range snap.shards {
		jobs <- shardJob{i, s}
	}
	close(jobs)

	workers := e.cfg.SearchWorkers
	if nShards < workers {
		workers = nShards
	}
	if workers < 1 {
		workers = 1
	}
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				if truncated.Load() {
					return
				}
				s := job.shard
				if !s.HasTrigrams(opts.Literal) {
					continue
				}
				if err := s.Mmap(); err != nil {
					log.Printf("mmap shard %s for search: %v", s.path, err)
					continue
				}
				sr := shardResult{fileCounts: make(map[string]int)}
				candidates := s.Candidates(opts.Literal)
				for _, id := range candidates {
					if truncated.Load() {
						break
					}
					fm := s.File(int(id))
					if _, isDirty := dirtySnap[fm.Path]; isDirty {
						continue
					}
					if opts.MaxFileSize > 0 && fm.Size > opts.MaxFileSize {
						continue
					}
					if opts.PathMatch != nil && !opts.PathMatch(fm.Path) {
						continue
					}
					inline := s.FileContent(int(id))
					matches := e.verifyFile(fm.Path, fm, mat, opts, inline)
					if len(matches) > 0 {
						sr.fileCounts[fm.Path] = len(matches)
						if !opts.FilesOnly {
							sr.matches = append(sr.matches, matches...)
						}
					}
				}
				s.Munmap()
				results[job.idx] = sr
			}
		}()
	}
	wg.Wait()

	// merge in shard order → deterministic output
	for _, sr := range results {
		for p, c := range sr.fileCounts {
			res.FileCounts[p] = c
		}
		if !opts.FilesOnly {
			res.Matches = append(res.Matches, sr.matches...)
		}
		if opts.Limit > 0 && len(res.Matches) >= opts.Limit {
			res.Matches = res.Matches[:opts.Limit]
			res.Truncated = true
			return res
		}
	}

	// 2. scan dirty files from disk
	var dirtyPaths []string
	for p, de := range dirtySnap {
		if !de.deleted {
			dirtyPaths = append(dirtyPaths, p)
		}
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
		matches := e.verifyFile(rel, fm, mat, opts, nil)
		if len(matches) > 0 {
			res.FileCounts[rel] = len(matches)
			if !opts.FilesOnly {
				res.Matches = append(res.Matches, matches...)
			}
		}
		if opts.Limit > 0 && len(res.Matches) >= opts.Limit {
			res.Truncated = true
			debug.FreeOSMemory()
			return res
		}
	}

	debug.FreeOSMemory()
	return res
}

func (e *DiskEngine) verifyFile(rel string, fm FileMeta, mat index.Matcher, opts index.SearchOpts, inline []byte) []index.Match {
	if inline != nil {
		return e.matchContent(rel, inline, mat, opts)
	}
	abs := filepath.Join(e.root, filepath.FromSlash(rel))
	f, err := os.Open(abs)
	if err != nil {
		return nil
	}
	defer f.Close()

	r := readerPool.Get().(*bufio.Reader)
	r.Reset(f)
	defer readerPool.Put(r)
	head, _ := r.Peek(8192)
	if bytes.IndexByte(head, 0) >= 0 {
		return nil
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

// matchContent scans inline shard content without opening a file.
func (e *DiskEngine) matchContent(rel string, content []byte, mat index.Matcher, opts index.SearchOpts) []index.Match {
	n := len(content)
	if n > 8192 {
		n = 8192
	}
	if bytes.IndexByte(content[:n], 0) >= 0 {
		return nil
	}
	var matches []index.Match
	lineNo := 0
	for len(content) > 0 {
		lineNo++
		var line []byte
		if idx := bytes.IndexByte(content, '\n'); idx >= 0 {
			line = content[:idx]
			content = content[idx+1:]
		} else {
			line = content
			content = nil
		}
		if loc := mat.FindFirst(line); loc != nil {
			m := index.Match{Path: rel, Line: lineNo, Col: loc[0], LineLen: len(line)}
			if opts.MaxColumns <= 0 || len(line) <= opts.MaxColumns {
				m.Text = string(line)
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
	return matches
}

// ---- Symbol methods (on-demand from disk) ----

func (e *DiskEngine) Defs(name string, fold bool, pathMatch func(string) bool) []index.DefHit {
	snap := e.snapshot.Load()
	want := name
	if fold {
		want = strings.ToLower(name)
	}

	var hits []index.DefHit

	// search shard symbol tables (no file content read needed)
	for _, s := range snap.shards {
		if err := s.Mmap(); err != nil {
			continue
		}
		for i := 0; i < s.NumFiles(); i++ {
			fm := s.File(i)
			if pathMatch != nil && !pathMatch(fm.Path) {
				continue
			}
			for _, d := range s.FileDefs(i) {
				got := d.Name
				if fold {
					got = strings.ToLower(got)
				}
				if got == want {
					text := e.readLine(fm.Path, d.Line)
					hits = append(hits, index.DefHit{Path: fm.Path, Def: d, Text: text})
				}
			}
		}
		s.Munmap()
	}

	// also check dirty (non-deleted) files from disk
	e.dirtyMu.Lock()
	var dirtyPaths []string
	for p, de := range e.dirty {
		if !de.deleted && (pathMatch == nil || pathMatch(p)) {
			dirtyPaths = append(dirtyPaths, p)
		}
	}
	e.dirtyMu.Unlock()
	for _, path := range dirtyPaths {
		abs := filepath.Join(e.root, filepath.FromSlash(path))
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
		for _, d := range symbol.Extract(path, content) {
			got := d.Name
			if fold {
				got = strings.ToLower(got)
			}
			if got == want {
				hits = append(hits, index.DefHit{Path: path, Def: d, Text: index.LineText(content, d.Line)})
			}
		}
	}
	return hits
}

func (e *DiskEngine) FileDefs(path string) ([]index.DefHit, bool) {
	abs := filepath.Join(e.root, filepath.FromSlash(path))
	content, err := os.ReadFile(abs)
	if err != nil {
		return nil, false
	}
	if decoded, ok := decodeUTF16(content); ok {
		content = decoded
	}
	if isBinary(content) {
		return nil, false
	}
	defs := symbol.Extract(path, content)
	if len(defs) == 0 {
		return nil, true
	}
	hits := make([]index.DefHit, 0, len(defs))
	for _, d := range defs {
		hits = append(hits, index.DefHit{Path: path, Def: d, Text: index.LineText(content, d.Line)})
	}
	return hits, true
}

func (e *DiskEngine) FilesContaining(lit string, pathMatch func(string) bool) []index.FileContent {
	snap := e.snapshot.Load()
	needle := []byte(lit)
	var out []index.FileContent
	seen := make(map[string]bool)

	for _, s := range snap.shards {
		if err := s.Mmap(); err != nil {
			continue
		}
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
		s.Munmap()
	}

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

// ---- Status ----

func (e *DiskEngine) NumFiles() int     { return int(e.totalFiles.Load()) }
func (e *DiskEngine) TotalBytes() int64 { return e.totalBytes.Load() }
func (e *DiskEngine) NumShards() int {
	return len(e.snapshot.Load().shards)
}

// ---- Meta for reconcile ----

func (e *DiskEngine) Meta(path string) (FileMeta, bool) {
	snap := e.snapshot.Load()
	si := snap.shardForPath(path)
	if si < 0 {
		return FileMeta{}, false
	}
	s := snap.shards[si]
	if err := s.Mmap(); err != nil {
		return FileMeta{}, false
	}
	defer s.Munmap()
	for i := 0; i < s.NumFiles(); i++ {
		fm := s.File(i)
		if fm.Path == path {
			return fm, true
		}
	}
	return FileMeta{}, false
}

func (e *DiskEngine) Paths() []string {
	snap := e.snapshot.Load()
	var out []string
	for _, s := range snap.shards {
		if err := s.Mmap(); err != nil {
			continue
		}
		for i := 0; i < s.NumFiles(); i++ {
			out = append(out, s.File(i).Path)
		}
		s.Munmap()
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
	dirtySnap := make(map[string]dirtyEntry, len(e.dirty))
	for k, v := range e.dirty {
		dirtySnap[k] = v
	}
	e.dirtyMu.Unlock()

	cur := e.snapshot.Load()
	oldShards := cur.shards
	seqNo := cur.seqNo

	// determine which shards are affected by dirty paths
	// count dirty files per shard to decide whether to rebuild
	dirtyPerShard := make(map[int]int)
	affected := make(map[int]bool)
	var newFiles []BuildFile
	for p, de := range dirtySnap {
		si := cur.shardForPath(p)
		if si >= 0 {
			dirtyPerShard[si]++
		} else if !de.deleted {
			abs := filepath.Join(e.root, filepath.FromSlash(p))
			fi, err := os.Stat(abs)
			if err == nil {
				newFiles = append(newFiles, BuildFile{Rel: p, Size: fi.Size(), MtimeNS: fi.ModTime().UnixNano()})
			}
		}
	}

	// only rebuild shards with enough dirty files to justify the I/O;
	// shards below threshold keep their dirty files in the dirty list
	// (scanned at query time) until more changes accumulate.
	threshold := e.cfg.RebuildThreshold
	var deferred []string
	for si, cnt := range dirtyPerShard {
		if cnt >= threshold {
			affected[si] = true
		} else {
			// keep these dirty entries for next cycle
			for p := range dirtySnap {
				if cur.shardForPath(p) == si {
					deferred = append(deferred, p)
				}
			}
		}
	}
	// also always rebuild if total dirty count is small (quick rebuild)
	if len(dirtySnap) <= threshold {
		for si := range dirtyPerShard {
			affected[si] = true
		}
		deferred = nil
	}

	newShards := make([]*Shard, len(oldShards))
	copy(newShards, oldShards)
	var totalBytes int64
	var oldFiles []string

	// count bytes from unchanged shards
	for si, s := range oldShards {
		if affected[si] {
			continue
		}
		if err := s.Mmap(); err != nil {
			continue
		}
		for i := 0; i < s.NumFiles(); i++ {
			totalBytes += s.File(i).Size
		}
		s.Munmap()
	}

	// rebuild affected shards
	for si, s := range oldShards {
		if !affected[si] {
			continue
		}
		b := NewBuilder(e.cfg.InlineKB)
		if err := s.Mmap(); err != nil {
			log.Printf("mmap shard for rebuild: %v", err)
			continue
		}
		for i := 0; i < s.NumFiles(); i++ {
			fm := s.File(i)
			if de, isDirty := dirtySnap[fm.Path]; isDirty {
				if de.deleted {
					continue
				}
				abs := filepath.Join(e.root, filepath.FromSlash(fm.Path))
				content, err := os.ReadFile(abs)
				if err != nil {
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
					continue
				}
				defs := symbol.Extract(fm.Path, content)
				b.AddWithDefs(fm.Path, fi.Size(), fi.ModTime().UnixNano(), content, defs)
				totalBytes += fi.Size()
			} else {
				abs := filepath.Join(e.root, filepath.FromSlash(fm.Path))
				content, err := os.ReadFile(abs)
				if err != nil {
					continue
				}
				if decoded, ok := decodeUTF16(content); ok {
					content = decoded
				}
				oldDefs := s.FileDefs(i)
				b.AddWithDefs(fm.Path, fm.Size, fm.MtimeNS, content, oldDefs)
				totalBytes += fm.Size
			}
		}
		s.Munmap()

		if b.NumFiles() == 0 {
			newShards[si] = nil
			oldFiles = append(oldFiles, s.path)
			continue
		}
		path := e.shardPath(seqNo)
		seqNo++
		if err := b.Write(path); err != nil {
			log.Printf("rebuild shard: %v", err)
			if merr := s.Mmap(); merr == nil {
				for i := 0; i < s.NumFiles(); i++ {
					totalBytes += s.File(i).Size
				}
				s.Munmap()
			}
			continue
		}
		ns, err := Open(path)
		if err != nil {
			log.Printf("open rebuilt shard: %v", err)
			continue
		}
		newShards[si] = ns
		oldFiles = append(oldFiles, s.path)
	}

	// build shard for new files
	if len(newFiles) > 0 {
		sort.Slice(newFiles, func(i, j int) bool { return newFiles[i].Rel < newFiles[j].Rel })
		b := NewBuilder(e.cfg.InlineKB)
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
			defs := symbol.Extract(f.Rel, content)
			b.AddWithDefs(f.Rel, f.Size, f.MtimeNS, content, defs)
			totalBytes += f.Size
		}
		if b.NumFiles() > 0 {
			path := e.shardPath(seqNo)
			seqNo++
			if err := b.Write(path); err == nil {
				ns, lerr := Open(path)
				if lerr == nil {
					newShards = append(newShards, ns)
				}
			}
		}
	}

	// compact out nil shards
	var compacted []*Shard
	for _, s := range newShards {
		if s != nil {
			compacted = append(compacted, s)
		}
	}
	// sort by pathMin to maintain ordering
	sort.Slice(compacted, func(i, j int) bool {
		return compacted[i].pathMin < compacted[j].pathMin
	})

	// count total files
	var totalFileCount int
	for _, s := range compacted {
		totalFileCount += s.NumFiles()
	}

	newSnap := &engineSnapshot{shards: compacted, seqNo: seqNo}
	e.snapshot.Store(newSnap)
	e.totalFiles.Store(int64(totalFileCount))
	e.totalBytes.Store(totalBytes)

	m := ManifestFromShards(compacted, seqNo)
	if err := WriteManifest(e.shardDir, m); err != nil {
		log.Printf("write manifest after rebuild: %v", err)
	}

	// clean up old pending files from previous cycles first, then
	// queue this cycle's old files for next cycle. This gives
	// in-flight searches time to finish with the old snapshot.
	e.cleanPending()
	e.pendingMu.Lock()
	e.pending = append(e.pending, oldFiles...)
	e.pendingMu.Unlock()

	// clear processed dirty entries (keep deferred ones for next cycle)
	deferredSet := make(map[string]bool, len(deferred))
	for _, p := range deferred {
		deferredSet[p] = true
	}
	e.dirtyMu.Lock()
	for p := range dirtySnap {
		if deferredSet[p] {
			continue
		}
		if cur, ok := e.dirty[p]; ok && cur == dirtySnap[p] {
			delete(e.dirty, p)
		}
	}
	e.dirtyMu.Unlock()

	log.Printf("shard rebuild: %d shards, %d files", len(compacted), totalFileCount)
}

func (e *DiskEngine) cleanPending() {
	e.pendingMu.Lock()
	defer e.pendingMu.Unlock()
	var remaining []string
	for _, p := range e.pending {
		if err := os.Remove(p); err != nil {
			remaining = append(remaining, p)
		}
	}
	e.pending = remaining
}

func (e *DiskEngine) readLine(rel string, lineNo int) string {
	abs := filepath.Join(e.root, filepath.FromSlash(rel))
	content, err := os.ReadFile(abs)
	if err != nil {
		return ""
	}
	return index.LineText(content, lineNo)
}

// ---- helpers ----

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
