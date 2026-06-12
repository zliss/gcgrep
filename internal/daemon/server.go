// Package daemon implements the resident indexing server. It owns one
// RootStore per indexed directory tree and serves search requests over
// the platform IPC transport using a JSON-lines protocol.
package daemon

import (
	"bufio"
	"encoding/json"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zliss/gcgrep/internal/conf"
	"github.com/zliss/gcgrep/internal/index"
	"github.com/zliss/gcgrep/internal/proto"
	"github.com/zliss/gcgrep/internal/symbol"
)

type Server struct {
	cfg      conf.Config
	mu       sync.Mutex
	stores   map[string]*RootStore
	cacheDir string
	quit     chan struct{} // closed when shutdown begins (stop accepting)
	stopped  chan struct{} // closed when all stores are saved
	stopOnce sync.Once
}

func NewServer(cacheDir string, cfg conf.Config) *Server {
	return &Server{
		cfg:      cfg,
		stores:   make(map[string]*RootStore),
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
func (sv *Server) store(path string) (*RootStore, error) {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	for root, s := range sv.stores {
		if path == root || strings.HasPrefix(path, root+string(filepath.Separator)) {
			return s, nil
		}
	}
	s, err := newRootStore(path, sv.cacheDir, sv.cfg)
	if err != nil {
		return nil, err
	}
	sv.stores[path] = s
	return s, nil
}

func (sv *Server) handle(conn net.Conn) {
	defer conn.Close()
	dec := json.NewDecoder(bufio.NewReader(conn))
	w := bufio.NewWriter(conn)
	enc := json.NewEncoder(w)
	send := func(ev proto.Event) error {
		if err := enc.Encode(ev); err != nil {
			return err
		}
		// flushing per match throttles high-hit queries on named pipes;
		// matches ride the bufio buffer and flush with the final event
		if ev.Type == "match" || ev.Type == "filecount" {
			return nil
		}
		return w.Flush()
	}
	var req proto.Request
	if err := dec.Decode(&req); err != nil {
		return
	}
	switch req.Op {
	case "search":
		sv.handleSearch(req, send)
	case "def", "refs", "symbols":
		sv.handleSymbol(req, send)
	case "status":
		sv.handleStatus(send)
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
	for root, s := range sv.stores {
		ev.Roots = append(ev.Roots, proto.RootStatus{
			Root: root, State: s.State(), Files: s.idx.NumFiles(),
			SizeMB:        int(s.totalBytes.Load() >> 20),
			SkippedLarge:  int(s.skippedLarge.Load()),
			SkippedBudget: int(s.skippedBudget.Load()),
			SkippedBinary: int(s.skippedBinary.Load()),
			SkippedError:  int(s.skippedError.Load()),
		})
	}
	sv.mu.Unlock()
	sort.Slice(ev.Roots, func(i, j int) bool { return ev.Roots[i].Root < ev.Roots[j].Root })
	_ = send(ev)
}

func (sv *Server) handleSearch(req proto.Request, send func(proto.Event) error) {
	start := time.Now()
	re, err := compilePattern(req)
	if err != nil {
		_ = send(proto.Event{Type: "error", Msg: "bad pattern: " + err.Error()})
		return
	}
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
		MaxColumns: maxCols,
		Literal:    index.ExtractLiteral(req.Pattern, req.Fixed),
		FilesOnly:  req.Files || req.Count,
		Limit:      req.Limit,
	}
	// pure literals skip the regex engine; the ASCII fold fast path is
	// only safe when the needle itself is ASCII
	if lit, ok := index.PlainLiteral(req.Pattern, req.Fixed); ok {
		if !req.NoCase {
			opts.PlainLiteral = lit
		} else if !index.HasNonASCII(lit) {
			opts.PlainLiteral = lit
			opts.FoldCase = true
		}
	}
	if pm, perr := globMatcher(req.Globs); perr != nil {
		_ = send(proto.Event{Type: "error", Msg: "bad glob: " + perr.Error()})
		return
	} else {
		opts.PathMatch = pm
	}
	if subPrefix != "" {
		inner := opts.PathMatch
		opts.PathMatch = func(p string) bool {
			if !strings.HasPrefix(p, subPrefix) {
				return false
			}
			return inner == nil || inner(strip(p))
		}
	}
	searchStart := time.Now()
	res := s.idx.Search(re, opts)
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
		for _, m := range res.Matches {
			ev := proto.Event{Type: "match", File: strip(m.Path), Line: m.Line, Text: m.Text}
			if m.Text == "" && m.LineLen > 0 {
				ev.Col, ev.LineLen = m.Col, m.LineLen
			}
			if send(ev) != nil {
				return
			}
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
	s, err := sv.store(req.Root)
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
	inSubtree := func(p string) bool { return subPrefix == "" || strings.HasPrefix(p, subPrefix) }

	emit := 0
	send1 := func(ev proto.Event) bool {
		emit++
		return send(ev) == nil
	}
	switch req.Op {
	case "def":
		hits := s.idx.Defs(req.Pattern, req.NoCase, inSubtree)
		sortDefHits(hits)
		for _, h := range hits {
			if !send1(proto.Event{Type: "match", File: strip(h.Path), Line: h.Def.Line,
				Text: h.Text, Kind: string(h.Def.Kind), Container: h.Def.Container, Name: h.Def.Name}) {
				return
			}
		}
	case "symbols":
		rel := filepath.ToSlash(filepath.Clean(req.Pattern))
		hits, found := s.idx.FileDefs(subPrefix + rel)
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
		files := s.idx.FilesContaining(req.Pattern, inSubtree)
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

func compilePattern(req proto.Request) (*regexp.Regexp, error) {
	pat := req.Pattern
	if req.Fixed {
		pat = regexp.QuoteMeta(pat)
	}
	if req.NoCase {
		pat = "(?i)" + pat
	}
	return regexp.Compile(pat)
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
