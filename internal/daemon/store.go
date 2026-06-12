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
	"github.com/zliss/gcgrep/internal/symbol"
	"github.com/zliss/gcgrep/internal/watch"
)

const (
	StateIndexing    = "indexing"
	StateReconciling = "reconciling"
	StateReady       = "ready"
)

// RootStore owns the index, watcher and persistence for one indexed root.
type RootStore struct {
	root    string
	idx     *index.Index
	ign     *ignore.Matcher
	watcher *watch.Watcher

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
	totalBytes    atomic.Int64
}

// newRootStore loads any persisted index, starts the watcher BEFORE the
// reconcile/scan pass (so changes during the pass are not lost), then
// builds or reconciles in the background.
func newRootStore(root, cacheDir string, cfg conf.Config) (*RootStore, error) {
	s := &RootStore{
		root:     root,
		cfg:      cfg,
		ign:      ignore.Load(root),
		cacheDir: cacheDir,
		ready:    make(chan struct{}),
		closed:   make(chan struct{}),
	}
	loaded := s.loadPersisted()
	if loaded == nil {
		s.idx = index.New(root)
		s.state.Store(StateIndexing)
	} else {
		s.idx = loaded
		s.state.Store(StateReconciling)
	}

	w, err := watch.New(root, s.ign.Ignored, cfg.Debounce)
	if err != nil {
		return nil, fmt.Errorf("watch %s: %w", root, err)
	}
	s.watcher = w
	go s.watchLoop()

	if loaded != nil {
		// queries wait for the reconcile pass: it is stat-only and fast,
		// and answering from the stale index would miss offline changes
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
	s.save()
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
	_ = filepath.WalkDir(s.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, rerr := filepath.Rel(s.root, path)
		if rerr != nil || rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if s.ign.Ignored(rel, true) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || s.ign.Ignored(rel, false) || isCookie(rel) {
			return nil
		}
		fi, ierr := d.Info()
		if ierr != nil {
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
		stale = append(stale, f.rel)
	}
	s.indexParallel(stale)
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

// indexFile reads and indexes one file; binary or oversized files are
// removed from the index if present. Read errors (racing deletes) likewise.
func (s *RootStore) indexFile(rel string) {
	abs := filepath.Join(s.root, filepath.FromSlash(rel))
	fi, err := os.Stat(abs)
	if err != nil || !fi.Mode().IsRegular() {
		s.removeTracked(rel)
		return
	}
	if fi.Size() > s.cfg.MaxFileSize() {
		s.skippedLarge.Add(1)
		s.removeTracked(rel)
		return
	}
	if s.cfg.MaxIndexMB > 0 && s.totalBytes.Load()+fi.Size() > int64(s.cfg.MaxIndexMB)<<20 {
		if s.skippedBudget.Add(1) == 1 {
			log.Printf("index budget GCGREP_MAX_INDEX_MB=%d reached on %s: further files skipped", s.cfg.MaxIndexMB, s.root)
		}
		s.removeTracked(rel)
		return
	}
	content, err := os.ReadFile(abs)
	if err != nil {
		s.removeTracked(rel)
		return
	}
	if isBinary(content) {
		s.removeTracked(rel)
		return
	}
	if old, ok := s.idx.Meta(rel); ok {
		s.totalBytes.Add(-old.Size)
	}
	s.totalBytes.Add(fi.Size())
	s.idx.Add(index.FileMeta{Path: rel, Size: fi.Size(), MtimeNS: fi.ModTime().UnixNano()}, content)
}

// removeTracked removes a file from the index keeping the byte budget
// accounting consistent.
func (s *RootStore) removeTracked(rel string) {
	if old, ok := s.idx.Meta(rel); ok {
		s.totalBytes.Add(-old.Size)
	}
	s.idx.Remove(rel)
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
	switch {
	case err != nil: // removed (or inaccessible): drop file or subtree
		if s.ign.Ignored(rel, true) && s.ign.Ignored(rel, false) {
			return
		}
		s.idx.Remove(rel)
		s.idx.RemovePrefix(rel)
	case fi.IsDir():
		if s.ign.Ignored(rel, true) {
			return
		}
		// new or moved-in directory: index its contents
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
	_ = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, rerr := filepath.Rel(s.root, path)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != relDir && s.ign.Ignored(rel, true) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() && !s.ign.Ignored(rel, false) {
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
	sum := sha256.Sum256([]byte(s.root))
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
