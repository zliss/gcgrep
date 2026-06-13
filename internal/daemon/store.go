package daemon

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zliss/gcgrep/internal/conf"
	"github.com/zliss/gcgrep/internal/ignore"
	"github.com/zliss/gcgrep/internal/index"
	"github.com/zliss/gcgrep/internal/shard"
	"github.com/zliss/gcgrep/internal/symbol"
	"github.com/zliss/gcgrep/internal/walkdir"
	"github.com/zliss/gcgrep/internal/watch"
)

const (
	StateIndexing    = "indexing"
	StateReconciling = "reconciling"
	StateReady       = "ready"
)

// RootStore owns the index, watcher and persistence for one indexed root.
// follow=true is a separate store variant (rg -L): the walk and the
// watcher both traverse symlinked directories.
type RootStore struct {
	root       string
	follow     bool
	engineType string            // "mem" or "disk"
	idx        *index.Index      // mem engine
	disk       *shard.DiskEngine // disk engine
	ign        *ignore.Matcher
	watcher    *watch.Watcher

	// stream is the manifest of searchable-but-not-indexed files
	// (large/binary/over-budget); see stream.go. In-memory only:
	// it is rebuilt by the scan/reconcile pass on daemon start.
	streamMu sync.Mutex
	stream   map[string]streamEntry

	state   atomic.Value // string
	indexed atomic.Int64
	total   atomic.Int64
	ready   chan struct{} // closed when first usable index is available

	saveMu    sync.Mutex
	saveTimer *time.Timer
	cacheDir  string
	closed    chan struct{}

	barrierMu sync.Mutex
	cookieSeq atomic.Int64

	cfg conf.Config
	// observability: files not indexed and why (surfaced via status)
	skippedLarge  atomic.Int64
	skippedBudget atomic.Int64
	skippedBinary atomic.Int64
	skippedError  atomic.Int64 // unreadable (permissions, racing deletes)
	totalBytes    atomic.Int64
}

// newRootStore loads any persisted index, starts the watcher BEFORE the
// reconcile/scan pass (so changes during the pass are not lost), then
// builds or reconciles in the background.
func newRootStore(root, cacheDir string, cfg conf.Config, follow bool) (*RootStore, error) {
	s := &RootStore{
		root:     root,
		follow:   follow,
		cfg:      cfg,
		ign:      ignore.Load(root),
		cacheDir: cacheDir,
		stream:   make(map[string]streamEntry),
		ready:    make(chan struct{}),
		closed:   make(chan struct{}),
	}

	// engine selection: decide whether to use disk shards
	engine := cfg.Engine
	if engine == "auto" {
		engine = "mem" // default; may upgrade to disk in fullScan after counting
	}
	s.engineType = engine

	if engine == "disk" {
		s.initDiskEngine()
	} else {
		loaded := s.loadPersisted()
		if loaded == nil {
			s.idx = index.New(root)
			s.state.Store(StateIndexing)
		} else {
			s.idx = loaded
			s.state.Store(StateReconciling)
		}
	}

	w, err := watch.New(root, s.ign.Ignored, cfg.Debounce, follow)
	if err != nil {
		return nil, fmt.Errorf("watch %s: %w", root, err)
	}
	s.watcher = w
	go s.watchLoop()

	if engine == "disk" {
		go func() {
			files := s.listFiles()
			s.total.Store(int64(len(files)))
			shardDir := s.shardDir()
			existing, _ := os.ReadDir(shardDir)
			hasShards := false
			for _, e := range existing {
				if strings.HasSuffix(e.Name(), ".idx") {
					hasShards = true
					break
				}
			}
			bf := make([]shard.BuildFile, len(files))
			for i, f := range files {
				bf[i] = shard.BuildFile{Rel: f.rel, Size: f.size, MtimeNS: f.mtimeNS}
			}
			if hasShards {
				s.state.Store(StateReconciling)
				s.disk.Reconcile(bf)
			} else {
				s.state.Store(StateIndexing)
				_ = s.disk.FullBuild(bf, func(n int) { s.indexed.Add(int64(n)) })
			}
			s.state.Store(StateReady)
			close(s.ready)
		}()
	} else if s.idx != nil && s.state.Load().(string) == StateReconciling {
		go func() {
			s.reconcile()
			s.state.Store(StateReady)
			close(s.ready)
		}()
	} else {
		go func() {
			s.fullScan()
			s.state.Store(StateReady)
			close(s.ready)
			s.scheduleSave()
		}()
	}
	return s, nil
}

func (s *RootStore) initDiskEngine() {
	dir := s.shardDir()
	_ = os.MkdirAll(dir, 0o755)
	s.disk = shard.NewDiskEngine(s.root, dir, s.cfg.Workers, s.cfg.RebuildInterval)
	s.state.Store(StateIndexing)
}

func (s *RootStore) shardDir() string {
	key := s.root
	if s.follow {
		key += "\x00follow"
	}
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(s.cacheDir, "shards-"+hex.EncodeToString(sum[:8]))
}

// IsDisk reports whether this store uses the disk shard engine.
func (s *RootStore) IsDisk() bool { return s.engineType == "disk" }

func (s *RootStore) State() string { return s.state.Load().(string) }

func (s *RootStore) Progress() (indexed, total int) {
	return int(s.indexed.Load()), int(s.total.Load())
}

// WaitReady blocks until the index is queryable or the store is closed.
func (s *RootStore) WaitReady() {
	select {
	case <-s.ready:
	case <-s.closed:
	}
}

func (s *RootStore) Close() {
	select {
	case <-s.closed:
		return
	default:
	}
	close(s.closed)
	s.watcher.Close()
	if s.disk != nil {
		s.disk.Close()
	}
	if s.idx != nil {
		s.save()
	}
}

// ---- scanning ----

type fileStat struct {
	rel     string
	size    int64
	mtimeNS int64
}

// listFiles walks root collecting indexable files with the stat data the
// walk already produced, so reconcile needs no second stat per file.
func (s *RootStore) listFiles() []fileStat {
	var out []fileStat
	_ = walkdir.Walk(s.root, s.follow, func(path string, isDir bool, fi os.FileInfo) error {
		rel, rerr := filepath.Rel(s.root, path)
		if rerr != nil || rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if isDir {
			if s.ign.Ignored(rel, true) {
				return fs.SkipDir
			}
			return nil
		}
		if s.ign.Ignored(rel, false) || isCookie(rel) {
			return nil
		}
		out = append(out, fileStat{rel: rel, size: fi.Size(), mtimeNS: fi.ModTime().UnixNano()})
		return nil
	})
	return out
}

func (s *RootStore) fullScan() {
	files := s.listFiles()
	s.total.Store(int64(len(files)))
	// auto engine selection: check total source size
	if s.cfg.Engine == "auto" {
		var totalMB int64
		for _, f := range files {
			totalMB += f.size
		}
		totalMB >>= 20
		if totalMB >= int64(s.cfg.DiskEngineMB) {
			log.Printf("source size %dMB >= GCGREP_DISK_ENGINE_MB=%d: switching to disk engine for %s", totalMB, s.cfg.DiskEngineMB, s.root)
			s.engineType = "disk"
			s.initDiskEngine()
			bf := make([]shard.BuildFile, len(files))
			for i, f := range files {
				bf[i] = shard.BuildFile{Rel: f.rel, Size: f.size, MtimeNS: f.mtimeNS}
			}
			_ = s.disk.FullBuild(bf, func(n int) { s.indexed.Add(int64(n)) })
			return
		}
	}
	rels := make([]string, len(files))
	for i, f := range files {
		rels[i] = f.rel
	}
	s.indexParallel(rels)
}

// indexParallel indexes files across CPU workers: trigram computation and
// file reads dominate and need no lock; index insertion serializes briefly.
func (s *RootStore) indexParallel(rels []string) {
	jobs := make(chan string)
	var wg sync.WaitGroup
	for i := 0; i < s.cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for rel := range jobs {
				s.indexFile(rel)
				s.indexed.Add(1)
			}
		}()
	}
	for _, rel := range rels {
		jobs <- rel
	}
	close(jobs)
	wg.Wait()
}

// reconcile compares disk state against the loaded index by mtime+size and
// fixes any drift accumulated while the daemon was down.
func (s *RootStore) reconcile() {
	files := s.listFiles()
	s.total.Store(int64(len(files)))
	onDisk := make(map[string]struct{}, len(files))
	var stale []string
	for _, f := range files {
		onDisk[f.rel] = struct{}{}
		if meta, ok := s.idx.Meta(f.rel); ok && f.size == meta.Size && f.mtimeNS == meta.MtimeNS {
			s.indexed.Add(1)
			continue
		}
		if e, ok := s.streamGet(f.rel); ok && f.size == e.size && f.mtimeNS == e.mtimeNS {
			s.indexed.Add(1) // unchanged stream-set file: no re-routing needed
			continue
		}
		stale = append(stale, f.rel)
	}
	s.indexParallel(stale)
	s.streamRetain(onDisk)
	changed := false
	for _, p := range s.idx.Paths() {
		if _, ok := onDisk[p]; !ok {
			s.idx.Remove(p)
			changed = true
		}
	}
	if changed || int(s.indexed.Load()) != s.idx.NumFiles() {
		s.scheduleSave()
	}
}

// indexFile reads and indexes one file. Oversized, over-budget and binary
// files are routed to the stream manifest (searchable via client-side
// scan) instead of being indexed. Read errors (racing deletes) drop the
// file from both sets.
func (s *RootStore) indexFile(rel string) {
	abs := filepath.Join(s.root, filepath.FromSlash(rel))
	fi, err := os.Stat(abs)
	if err != nil || !fi.Mode().IsRegular() {
		if err != nil && !os.IsNotExist(err) {
			s.skippedError.Add(1)
		}
		s.removeTracked(rel)
		return
	}
	size, mtimeNS := fi.Size(), fi.ModTime().UnixNano()
	if size > s.cfg.MaxFileSize() {
		s.skippedLarge.Add(1)
		s.toStream(rel, size, mtimeNS)
		return
	}
	if !s.reserveBytes(size) {
		if s.skippedBudget.Add(1) == 1 {
			log.Printf("index budget GCGREP_MAX_INDEX_MB=%d reached on %s: further files go to the stream set", s.cfg.MaxIndexMB, s.root)
		}
		s.toStream(rel, size, mtimeNS)
		return
	}
	content, err := os.ReadFile(abs)
	if err != nil {
		s.totalBytes.Add(-size) // release the reservation
		if !os.IsNotExist(err) {
			s.skippedError.Add(1)
		}
		s.removeTracked(rel)
		return
	}
	if decoded, ok := decodeUTF16(content); ok {
		content = decoded // UTF-16 with BOM: index the UTF-8 transcoding
	}
	if isBinary(content) {
		s.totalBytes.Add(-size) // release the reservation
		s.skippedBinary.Add(1)
		s.toStream(rel, size, mtimeNS)
		return
	}
	if old, ok := s.idx.Meta(rel); ok {
		s.totalBytes.Add(-old.Size)
	}
	s.streamDelete(rel) // may have shrunk / become text
	// meta carries the ON-DISK size/mtime (reconcile compares against
	// stat), even when content is a transcoding of different length;
	// the byte reservation above already accounts for size
	s.idx.Add(index.FileMeta{Path: rel, Size: size, MtimeNS: mtimeNS}, content)
}

// reserveBytes atomically claims n bytes of the per-root index budget.
// CAS instead of check-then-add: parallel workers racing a plain read
// would all pass the check and overshoot the budget together.
func (s *RootStore) reserveBytes(n int64) bool {
	if s.cfg.MaxIndexMB <= 0 {
		s.totalBytes.Add(n)
		return true
	}
	budget := int64(s.cfg.MaxIndexMB) << 20
	for {
		cur := s.totalBytes.Load()
		if cur+n > budget {
			return false
		}
		if s.totalBytes.CompareAndSwap(cur, cur+n) {
			return true
		}
	}
}

// toStream moves a file from the index to the stream manifest.
func (s *RootStore) toStream(rel string, size, mtimeNS int64) {
	if old, ok := s.idx.Meta(rel); ok {
		s.totalBytes.Add(-old.Size)
	}
	s.idx.Remove(rel)
	s.streamPut(rel, size, mtimeNS)
}

// removeTracked removes a file from index and stream manifest, keeping
// the byte budget accounting consistent.
func (s *RootStore) removeTracked(rel string) {
	if old, ok := s.idx.Meta(rel); ok {
		s.totalBytes.Add(-old.Size)
	}
	s.idx.Remove(rel)
	s.streamDelete(rel)
}

func isCookie(rel string) bool {
	base := rel
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		base = rel[i+1:]
	}
	return strings.HasPrefix(base, watch.CookiePrefix)
}

func isBinary(content []byte) bool {
	n := len(content)
	if n > 8192 {
		n = 8192
	}
	return bytes.IndexByte(content[:n], 0) >= 0
}

// ---- watching ----

func (s *RootStore) watchLoop() {
	for batch := range s.watcher.C() {
		if batch.Rescan {
			log.Printf("watch overflow on %s: reconciling", s.root)
			s.reconcile()
		} else {
			for _, abs := range batch.Paths {
				s.applyChange(abs)
			}
		}
		if len(batch.Paths) > 0 || batch.Rescan {
			s.scheduleSave()
		}
		if batch.Done != nil {
			close(batch.Done)
		}
	}
}

// Barrier guarantees read-after-write consistency: a search issued after a
// file write observes that write. It drops a cookie file into the root and
// waits for its create event — proof that the OS queue up to the write has
// been drained — then synchronously flushes and applies the debounce queue.
// On an unwritable root it degrades to flushing already-delivered events.
func (s *RootStore) Barrier(timeout time.Duration) {
	if s.State() != StateReady {
		return // initial scan / reconcile reads current disk state anyway
	}
	s.barrierMu.Lock()
	defer s.barrierMu.Unlock()
	// drain cookies left over from timed-out barriers
	for {
		select {
		case <-s.watcher.Cookies():
			continue
		default:
		}
		break
	}
	name := fmt.Sprintf("%s%d-%d", watch.CookiePrefix, os.Getpid(), s.cookieSeq.Add(1))
	path := filepath.Join(s.root, name)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		s.watcher.Flush(timeout)
		return
	}
	defer os.Remove(path)
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
wait:
	for {
		select {
		case got := <-s.watcher.Cookies():
			if got == name {
				break wait
			}
		case <-deadline.C:
			log.Printf("barrier cookie timeout on %s", s.root)
			break wait
		case <-s.closed:
			return
		}
	}
	s.watcher.Flush(timeout)
}

func (s *RootStore) applyChange(abs string) {
	rel, err := filepath.Rel(s.root, abs)
	if err != nil {
		return
	}
	rel = filepath.ToSlash(rel)
	if isCookie(rel) {
		return
	}
	if rel == "." || rel == ignore.ControlFile {
		// changed exclusion rules require a reload + reconcile to apply
		if rel == ignore.ControlFile {
			s.ign = ignore.Load(s.root)
			go func() { s.reconcile(); s.scheduleSave() }()
		}
		return
	}
	fi, err := os.Lstat(abs)
	if s.disk != nil {
		// disk engine: mark dirty, the rebuild cycle handles the rest
		switch {
		case err != nil:
			if s.ign.Ignored(rel, true) && s.ign.Ignored(rel, false) {
				return
			}
			s.disk.MarkDeleted(rel)
			s.disk.MarkDeletedPrefix(rel)
		case fi.IsDir():
			if s.ign.Ignored(rel, true) {
				return
			}
			for _, sub := range s.listUnder(rel) {
				s.disk.MarkDirty(sub)
			}
		case fi.Mode().IsRegular():
			if s.ign.Ignored(rel, false) {
				return
			}
			s.disk.MarkDirty(rel)
		}
		return
	}
	switch {
	case err != nil: // removed (or inaccessible): drop file or subtree
		if s.ign.Ignored(rel, true) && s.ign.Ignored(rel, false) {
			return
		}
		s.idx.Remove(rel)
		s.idx.RemovePrefix(rel)
		s.streamDelete(rel)
		s.streamDeletePrefix(rel)
	case fi.IsDir():
		if s.ign.Ignored(rel, true) {
			return
		}
		for _, sub := range s.listUnder(rel) {
			s.indexFile(sub)
		}
	case fi.Mode().IsRegular():
		if s.ign.Ignored(rel, false) {
			return
		}
		s.indexFile(rel)
	}
}

// listUnder lists indexable files under the given relative directory.
func (s *RootStore) listUnder(relDir string) []string {
	var out []string
	base := filepath.Join(s.root, filepath.FromSlash(relDir))
	_ = walkdir.Walk(base, s.follow, func(path string, isDir bool, fi os.FileInfo) error {
		rel, rerr := filepath.Rel(s.root, path)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if isDir {
			if rel != relDir && s.ign.Ignored(rel, true) {
				return fs.SkipDir
			}
			return nil
		}
		if !s.ign.Ignored(rel, false) {
			out = append(out, rel)
		}
		return nil
	})
	return out
}

// ---- persistence ----

type persisted struct {
	Version  string
	Root     string
	Metas    []index.FileMeta
	Contents [][]byte
	Defs     [][]symbol.Def
}

// persistVersion 2: added symbol definitions. Older files are discarded
// and rebuilt by a full scan.
const persistVersion = "2"

func (s *RootStore) indexPath() string {
	key := s.root
	if s.follow {
		key += "\x00follow" // -L variant persists separately
	}
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(s.cacheDir, "index-"+hex.EncodeToString(sum[:8])+".gob.gz")
}

func (s *RootStore) loadPersisted() *index.Index {
	f, err := os.Open(s.indexPath())
	if err != nil {
		return nil
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil
	}
	var p persisted
	if err := gob.NewDecoder(gz).Decode(&p); err != nil || p.Version != persistVersion || p.Root != s.root {
		return nil
	}
	// trigram recomputation dominates load time: parallelize it
	idx := index.New(s.root)
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < s.cfg.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				idx.AddWithKeys(p.Metas[i], p.Contents[i], index.TrigramKeys(p.Contents[i]), p.Defs[i])
			}
		}()
	}
	for i := range p.Metas {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	var total int64
	for _, m := range p.Metas {
		total += m.Size
	}
	s.totalBytes.Store(total)
	return idx
}

// scheduleSave debounces persistence so bursts of changes write once.
func (s *RootStore) scheduleSave() {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	if s.saveTimer != nil {
		s.saveTimer.Stop()
	}
	s.saveTimer = time.AfterFunc(s.cfg.SaveDelay, s.save)
}

func (s *RootStore) save() {
	metas, contents, defs := s.idx.Snapshot()
	p := persisted{Version: persistVersion, Root: s.root, Metas: metas, Contents: contents, Defs: defs}
	tmp := s.indexPath() + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		log.Printf("save %s: %v", s.root, err)
		return
	}
	gz, _ := gzip.NewWriterLevel(f, gzip.BestSpeed)
	err = gob.NewEncoder(gz).Encode(&p)
	if cerr := gz.Close(); err == nil {
		err = cerr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, s.indexPath())
	}
	if err != nil {
		log.Printf("save %s: %v", s.root, err)
		_ = os.Remove(tmp)
	}
}
