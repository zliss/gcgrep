// Package daemon implements the resident indexing server. It owns one
// RootStore per indexed directory tree and serves search requests over
// the platform IPC transport using a JSON-lines protocol.
package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zliss/gcgrep/internal/conf"
	"github.com/zliss/gcgrep/internal/ignore"
	"github.com/zliss/gcgrep/internal/index"
	"github.com/zliss/gcgrep/internal/proto"
	"github.com/zliss/gcgrep/internal/symbol"
)

// storeKey identifies a store: the same root with and without -L
// (follow symlinks) indexes different file sets.
type storeKey struct {
	root   string
	follow bool
}

type Server struct {
	cfg      conf.Config
	mu       sync.Mutex
	stores   map[storeKey]*RootStore
	cacheDir string
	quit     chan struct{} // closed when shutdown begins (stop accepting)
	stopped  chan struct{} // closed when all stores are saved
	stopOnce sync.Once
}

func NewServer(cacheDir string, cfg conf.Config) *Server {
	return &Server{
		cfg:      cfg,
		stores:   make(map[storeKey]*RootStore),
		cacheDir: cacheDir,
		quit:     make(chan struct{}),
		stopped:  make(chan struct{}),
	}
}

// Serve accepts connections until Stop. It is the daemon main loop.
func (sv *Server) Serve(l net.Listener) error {
	go func() {
		<-sv.quit
		l.Close()
	}()
	for {
		conn, err := l.Accept()
		if err != nil {
			select {
			case <-sv.quit:
				<-sv.stopped // index persistence must finish before exit
				return nil
			default:
				return err
			}
		}
		go sv.handle(conn)
	}
}

// Quit returns a channel closed when a stop request has been processed.
func (sv *Server) Quit() <-chan struct{} { return sv.quit }

func (sv *Server) stopAll() {
	sv.stopOnce.Do(sv.doStop)
}

func (sv *Server) doStop() {
	// stop accepting new connections first: persisting large indexes can
	// take seconds and new clients must spawn a fresh daemon, not reach a
	// half-shut-down one
	close(sv.quit)
	sv.mu.Lock()
	stores := make([]*RootStore, 0, len(sv.stores))
	for _, s := range sv.stores {
		stores = append(stores, s)
	}
	sv.mu.Unlock()
	for _, s := range stores {
		s.Close()
	}
	close(sv.stopped)
}

// store returns the RootStore covering path: an existing root that equals
// or contains path is reused; otherwise a new store is created for path.
// allowNested bypasses the nested-root check.
func (sv *Server) store(path string, follow, allowNested bool) (*RootStore, error) {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	for key, s := range sv.stores {
		if key.follow == follow && (path == key.root || strings.HasPrefix(path, key.root+string(filepath.Separator))) {
			return s, nil
		}
	}
	if !allowNested {
		var nested []string
		sep := string(filepath.Separator)
		for key := range sv.stores {
			if key.follow != follow {
				continue
			}
			if strings.HasPrefix(key.root, path+sep) {
				nested = append(nested, key.root)
			}
		}
		if len(nested) > 0 {
			sort.Strings(nested)
			return nil, fmt.Errorf("root %s contains already-indexed sub-root(s): %s; use 'gcgrep forget' to remove them or --allow-nested to proceed", path, strings.Join(nested, ", "))
		}
	}
	s, err := newRootStore(path, sv.cacheDir, sv.cfg, follow)
	if err != nil {
		return nil, err
	}
	sv.stores[storeKey{root: path, follow: follow}] = s
	return s, nil
}

func (sv *Server) handle(conn net.Conn) {
	defer conn.Close()
	dec := json.NewDecoder(bufio.NewReaderSize(conn, 64*1024))
	w := bufio.NewWriterSize(conn, 256*1024)
	enc := json.NewEncoder(w)
	send := func(ev proto.Event) error {
		if ev.Type == "done" || ev.Type == "status" {
			ev.V = proto.Version // lets a mismatched client warn
		}
		if err := enc.Encode(ev); err != nil {
			return err
		}
		// matches and filecounts ride the bufio buffer; other events
		// (done, progress, error, status) flush immediately.
		if ev.Type == "match" || ev.Type == "filecount" {
			return nil
		}
		return w.Flush()
	}
	flush := func() error { return w.Flush() }
	var req proto.Request
	if err := dec.Decode(&req); err != nil {
		return
	}
	switch req.Op {
	case "search":
		sv.handleSearch(req, send, flush)
	case "def", "refs", "symbols":
		sv.handleSymbol(req, send)
	case "status":
		sv.handleStatus(send)
	case "forget":
		sv.handleForget(req, send)
	case "stop":
		_ = send(proto.Event{Type: "done"})
		sv.stopAll()
	default:
		_ = send(proto.Event{Type: "error", Msg: "unknown op: " + req.Op})
	}
}

func (sv *Server) handleStatus(send func(proto.Event) error) {
	sv.mu.Lock()
	ev := proto.Event{Type: "status", PID: os.Getpid()}
	for key, s := range sv.stores {
		numFiles := 0
		sizeMB := int(s.totalBytes.Load() >> 20)
		engine := "mem"
		if s.disk != nil {
			numFiles = s.disk.NumFiles()
			sizeMB = int(s.disk.TotalBytes() >> 20)
			engine = "disk"
		} else if s.idx != nil {
			numFiles = s.idx.NumFiles()
		}
		cacheDir, cacheSizeBytes := s.CacheInfo()
		ev.Roots = append(ev.Roots, proto.RootStatus{
			Root: key.root, State: s.State(), Files: numFiles,
			SizeMB:        sizeMB,
			SkippedLarge:  int(s.skippedLarge.Load()),
			SkippedBudget: int(s.skippedBudget.Load()),
			SkippedBinary: int(s.skippedBinary.Load()),
			SkippedError:  int(s.skippedError.Load()),
			StreamFiles:   s.streamCount(),
			Follow:        key.follow,
			Engine:        engine,
			CacheDir:      cacheDir,
			CacheSizeMB:   int(cacheSizeBytes >> 20),
		})
	}
	sv.mu.Unlock()
	sort.Slice(ev.Roots, func(i, j int) bool { return ev.Roots[i].Root < ev.Roots[j].Root })
	_ = send(ev)
}

func (sv *Server) handleSearch(req proto.Request, send func(proto.Event) error, flush func() error) {
	start := time.Now()
	m, err := index.MatcherFor(req.Pattern, req.Fixed, req.NoCase)
	if err != nil {
		_ = send(proto.Event{Type: "error", Msg: "bad pattern: " + err.Error()})
		return
	}
	re := m.Re
	s, subPrefix, barrierMS, ok := sv.prologue(req, send)
	if !ok {
		return
	}
	strip := func(p string) string { return strings.TrimPrefix(p, subPrefix) }

	maxCols := req.MaxColumns
	switch {
	case maxCols == 0:
		maxCols = sv.cfg.MaxColumns // 0 = unlimited, matching grep/rg
	case maxCols < 0:
		maxCols = 0 // explicit unlimited
	}
	opts := index.SearchOpts{
		MaxColumns:   maxCols,
		Literal:      index.ExtractLiteral(req.Pattern, req.Fixed),
		FilesOnly:    req.Files || req.Count,
		Limit:        req.Limit,
		PlainLiteral: m.PlainLit,
		FoldCase:     m.Fold,
		MaxFileSize:  req.MaxFilesize,
	}
	pm, perr := globMatcher(req.Globs)
	if perr != nil {
		_ = send(proto.Event{Type: "error", Msg: "bad glob: " + perr.Error()})
		return
	}
	exIn, eierr := excludeIncludeMatcher(req.Exclude, req.Include)
	if eierr != nil {
		_ = send(proto.Event{Type: "error", Msg: "bad exclude/include glob: " + eierr.Error()})
		return
	}
	reqOK := pm
	if exIn != nil {
		inner := reqOK
		reqOK = func(p string) bool { return exIn(p) && (inner == nil || inner(p)) }
	}
	if !req.Hidden {
		inner := reqOK
		reqOK = func(p string) bool { return !hiddenPath(p) && (inner == nil || inner(p)) }
	}
	opts.PathMatch = reqOK
	if subPrefix != "" {
		opts.PathMatch = func(p string) bool {
			if !strings.HasPrefix(p, subPrefix) {
				return false
			}
			return reqOK == nil || reqOK(strip(p))
		}
	}
	if req.Gitignore {
		gitTree := ignore.NewGitignoreTree(s.root)
		incMatch := buildIncludeMatcher(req.Include)
		inner := opts.PathMatch
		opts.PathMatch = func(p string) bool {
			stripped := p
			if subPrefix != "" {
				stripped = strings.TrimPrefix(p, subPrefix)
			}
			if gitTree.Ignored(p, false) && !incMatch(stripped) {
				return false
			}
			return inner == nil || inner(p)
		}
	}
	searchStart := time.Now()
	var res index.SearchResult
	if s.disk != nil {
		res = s.disk.Search(re, opts)
	} else {
		res = s.idx.Search(re, opts)
	}
	searchMS := time.Since(searchStart).Milliseconds()

	switch {
	case req.Count, req.Files:
		paths := make([]string, 0, len(res.FileCounts))
		for p := range res.FileCounts {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for _, p := range paths {
			ev := proto.Event{Type: "filecount", File: strip(p)}
			if req.Count {
				ev.Count = res.FileCounts[p]
			}
			if send(ev) != nil {
				return
			}
		}
	default:
		sort.Slice(res.Matches, func(i, j int) bool {
			a, b := res.Matches[i], res.Matches[j]
			if a.Path != b.Path {
				return a.Path < b.Path
			}
			return a.Line < b.Line
		})
		for i, m := range res.Matches {
			ev := proto.Event{Type: "match", File: strip(m.Path), Line: m.Line, Text: m.Text}
			if m.Text == "" && m.LineLen > 0 {
				ev.Col, ev.LineLen = m.Col, m.LineLen
			}
			if send(ev) != nil {
				return
			}
			if (i+1)%1000 == 0 {
				_ = flush()
			}
		}
	}
	_ = flush()
	// stream-set files passing the same filters are announced for the
	// client to scan from disk (the daemon holds only their manifest)
	for _, sf := range s.StreamList(req.MaxFilesize, opts.PathMatch) {
		if send(proto.Event{Type: "streamfile", File: strip(sf.Rel), Size: sf.Size}) != nil {
			return
		}
	}
	total := 0
	for _, c := range res.FileCounts {
		total += c
	}
	_ = send(proto.Event{Type: "done", Matches: total, FileHits: len(res.FileCounts),
		DurMS: time.Since(start).Milliseconds(), BarrierMS: barrierMS, SearchMS: searchMS,
		Truncated: res.Truncated})
}

// prologue resolves the store for req.Root, waits for readiness streaming
// progress, and applies the read-after-write barrier. Returns the store
// and the subtree prefix ("" when req.Root is the store root), or ok=false
// if an error was already sent / the client left.
func (sv *Server) prologue(req proto.Request, send func(proto.Event) error) (s *RootStore, subPrefix string, barrierMS int64, ok bool) {
	s, err := sv.store(req.Root, req.Follow, req.AllowNested)
	if err != nil {
		_ = send(proto.Event{Type: "error", Msg: err.Error()})
		return nil, "", 0, false
	}
	if !streamUntilReady(s, send) {
		return nil, "", 0, false
	}
	if !req.NoSync {
		t := time.Now()
		s.Barrier(sv.cfg.BarrierTimeout)
		barrierMS = time.Since(t).Milliseconds()
	}
	if rel, rerr := filepath.Rel(s.root, req.Root); rerr == nil && rel != "." {
		subPrefix = filepath.ToSlash(rel) + "/"
	}
	return s, subPrefix, barrierMS, true
}

// handleSymbol serves def (find definitions by name), refs (candidate
// references by name) and symbols (all definitions in one file).
func (sv *Server) handleSymbol(req proto.Request, send func(proto.Event) error) {
	start := time.Now()
	s, subPrefix, barrierMS, ok := sv.prologue(req, send)
	if !ok {
		return
	}
	strip := func(p string) string { return strings.TrimPrefix(p, subPrefix) }
	inSubtree := func(p string) bool {
		if subPrefix != "" && !strings.HasPrefix(p, subPrefix) {
			return false
		}
		return req.Hidden || !hiddenPath(strings.TrimPrefix(p, subPrefix))
	}

	emit := 0
	send1 := func(ev proto.Event) bool {
		emit++
		return send(ev) == nil
	}
	switch req.Op {
	case "def":
		var hits []index.DefHit
		if s.disk != nil {
			hits = s.disk.Defs(req.Pattern, req.NoCase, inSubtree)
		} else {
			hits = s.idx.Defs(req.Pattern, req.NoCase, inSubtree)
		}
		sortDefHits(hits)
		for _, h := range hits {
			if !send1(proto.Event{Type: "match", File: strip(h.Path), Line: h.Def.Line,
				Text: h.Text, Kind: string(h.Def.Kind), Container: h.Def.Container, Name: h.Def.Name}) {
				return
			}
		}
	case "symbols":
		rel := filepath.ToSlash(filepath.Clean(req.Pattern))
		var hits []index.DefHit
		var found bool
		if s.disk != nil {
			hits, found = s.disk.FileDefs(subPrefix + rel)
		} else {
			hits, found = s.idx.FileDefs(subPrefix + rel)
		}
		if !found {
			_ = send(proto.Event{Type: "error", Msg: "file not indexed: " + req.Pattern})
			return
		}
		for _, h := range hits {
			if !send1(proto.Event{Type: "match", File: strip(h.Path), Line: h.Def.Line,
				Text: h.Text, Kind: string(h.Def.Kind), Container: h.Def.Container, Name: h.Def.Name}) {
				return
			}
		}
	case "refs":
		var files []index.FileContent
		if s.disk != nil {
			files = s.disk.FilesContaining(req.Pattern, inSubtree)
		} else {
			files = s.idx.FilesContaining(req.Pattern, inSubtree)
		}
		sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
		for _, fc := range files {
			for _, r := range symbol.Refs(fc.Path, fc.Content, req.Pattern) {
				kind := "ref"
				if r.IsCall {
					kind = "call"
				}
				if !send1(proto.Event{Type: "match", File: strip(fc.Path), Line: r.Line,
					Text: index.LineText(fc.Content, r.Line), Kind: kind, Name: req.Pattern}) {
					return
				}
				if req.Limit > 0 && emit >= req.Limit {
					_ = send(proto.Event{Type: "done", Matches: emit, DurMS: time.Since(start).Milliseconds(), BarrierMS: barrierMS, Truncated: true})
					return
				}
			}
		}
	}
	_ = send(proto.Event{Type: "done", Matches: emit, DurMS: time.Since(start).Milliseconds(), BarrierMS: barrierMS})
}

func sortDefHits(hits []index.DefHit) {
	sort.Slice(hits, func(i, j int) bool {
		a, b := hits[i], hits[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Def.Line < b.Def.Line
	})
}

func (sv *Server) handleForget(req proto.Request, send func(proto.Event) error) {
	sv.mu.Lock()
	var found *RootStore
	var foundKey storeKey
	for key, s := range sv.stores {
		if key.root == req.Root {
			found = s
			foundKey = key
			break
		}
	}
	if found != nil {
		delete(sv.stores, foundKey)
	}
	sv.mu.Unlock()
	if found != nil {
		found.Close()
		found.DeleteCache()
		_ = send(proto.Event{Type: "done", Msg: "forgot " + req.Root})
	} else {
		_ = send(proto.Event{Type: "done", Msg: "not indexed: " + req.Root})
	}
}

// streamUntilReady emits progress events while the initial scan runs.
// Returns false if the client disconnected.
func streamUntilReady(s *RootStore, send func(proto.Event) error) bool {
	if s.State() == StateReady {
		return true
	}
	tick := time.NewTicker(progressIntervalOf(s))
	defer tick.Stop()
	for {
		select {
		case <-s.ready:
			return true
		case <-s.closed:
			return false
		case <-tick.C:
			indexed, total := s.Progress()
			if send(proto.Event{Type: "progress", Stage: s.State(), Indexed: indexed, Total: total}) != nil {
				return false
			}
		}
	}
}

func progressIntervalOf(s *RootStore) time.Duration {
	if s.cfg.ProgressInterval > 0 {
		return s.cfg.ProgressInterval
	}
	return 300 * time.Millisecond
}

// hiddenPath reports whether any segment of the slash-separated relative
// path starts with a dot (rg's default hidden-file exclusion).
func hiddenPath(rel string) bool {
	for len(rel) > 0 {
		if rel[0] == '.' {
			return true
		}
		i := strings.IndexByte(rel, '/')
		if i < 0 {
			return false
		}
		rel = rel[i+1:]
	}
	return false
}

// globMatcher matches a relative slashed path if any glob matches either
// the full path or its basename (ripgrep-like convenience).
func globMatcher(globs []string) (func(string) bool, error) {
	if len(globs) == 0 {
		return nil, nil
	}
	for _, g := range globs {
		if _, err := filepath.Match(g, "x"); err != nil {
			return nil, err
		}
	}
	return func(p string) bool {
		base := p
		if i := strings.LastIndexByte(p, '/'); i >= 0 {
			base = p[i+1:]
		}
		for _, g := range globs {
			if ok, _ := filepath.Match(g, p); ok {
				return true
			}
			if ok, _ := filepath.Match(g, base); ok {
				return true
			}
		}
		return false
	}, nil
}

// globMatchAny reports whether any glob matches the full path, its basename,
// or (for dir/* patterns) any descendant under that directory prefix.
func globMatchAny(globs []string, p string) bool {
	base := p
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		base = p[i+1:]
	}
	for _, g := range globs {
		if ok, _ := filepath.Match(g, p); ok {
			return true
		}
		if ok, _ := filepath.Match(g, base); ok {
			return true
		}
		if strings.HasSuffix(g, "/*") {
			if strings.HasPrefix(p, g[:len(g)-1]) {
				return true
			}
		}
	}
	return false
}

// buildIncludeMatcher returns a func that reports whether a path matches
// any --include glob. Returns a no-op (always false) when globs is empty.
func buildIncludeMatcher(globs []string) func(string) bool {
	if len(globs) == 0 {
		return func(string) bool { return false }
	}
	return func(p string) bool { return globMatchAny(globs, p) }
}

// excludeIncludeMatcher builds a path filter from --exclude and --include
// globs. Include overrides exclude: a path matching both is kept.
func excludeIncludeMatcher(exclude, include []string) (func(string) bool, error) {
	if len(exclude) == 0 && len(include) == 0 {
		return nil, nil
	}
	for _, g := range exclude {
		if _, err := filepath.Match(g, "x"); err != nil {
			return nil, err
		}
	}
	for _, g := range include {
		if _, err := filepath.Match(g, "x"); err != nil {
			return nil, err
		}
	}
	matchAny := func(globs []string, p string) bool {
		return globMatchAny(globs, p)
	}
	return func(p string) bool {
		if len(include) > 0 && matchAny(include, p) {
			return true
		}
		if len(exclude) > 0 && matchAny(exclude, p) {
			return false
		}
		return true
	}, nil
}

// Run starts the daemon: logs to logPath and serves on the given listener.
func Run(l net.Listener, cacheDir, logPath string) error {
	if logPath != "" {
		if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
			log.SetOutput(f)
			defer f.Close()
		}
	}
	log.Printf("gcgrep daemon %s starting, pid=%d", proto.Version, os.Getpid())
	cfg := conf.Load()
	if cfg.Priority != "normal" {
		if perr := lowerPriority(); perr != nil {
			log.Printf("lowering process priority: %v", perr)
		}
	}
	if ov := conf.Overrides(); len(ov) > 0 {
		log.Printf("config overrides: %v", ov)
	}
	sv := NewServer(cacheDir, cfg)
	// persist indexes on SIGTERM/SIGINT (service managers, pkill)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case s := <-sig:
			log.Printf("signal %v: saving and exiting", s)
			sv.stopAll()
		case <-sv.quit:
		}
	}()
	err := sv.Serve(l)
	log.Printf("gcgrep daemon exiting: %v", err)
	return err
}
