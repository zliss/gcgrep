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

	"github.com/zliss/gcgrep/internal/index"
	"github.com/zliss/gcgrep/internal/proto"
)

const (
	progressInterval = 300 * time.Millisecond
	barrierTimeout   = 2 * time.Second
)

type Server struct {
	mu       sync.Mutex
	stores   map[string]*RootStore
	cacheDir string
	quit     chan struct{} // closed when shutdown begins (stop accepting)
	stopped  chan struct{} // closed when all stores are saved
	stopOnce sync.Once
}

func NewServer(cacheDir string) *Server {
	return &Server{
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
	s, err := newRootStore(path, sv.cacheDir)
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
		return w.Flush()
	}
	var req proto.Request
	if err := dec.Decode(&req); err != nil {
		return
	}
	switch req.Op {
	case "search":
		sv.handleSearch(req, send)
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
		ev.Roots = append(ev.Roots, proto.RootStatus{Root: root, State: s.State(), Files: s.idx.NumFiles()})
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
	s, err := sv.store(req.Root)
	if err != nil {
		_ = send(proto.Event{Type: "error", Msg: err.Error()})
		return
	}
	if !streamUntilReady(s, send) {
		return // client went away during indexing
	}
	if !req.NoSync {
		// read-after-write consistency: searches observe all writes that
		// happened before the request (cookie-file barrier, watchman-style)
		s.Barrier(barrierTimeout)
	}

	// the store may cover an ancestor of the requested path: restrict to
	// the requested subtree and report paths relative to it
	subPrefix := ""
	if rel, rerr := filepath.Rel(s.root, req.Root); rerr == nil && rel != "." {
		subPrefix = filepath.ToSlash(rel) + "/"
	}
	strip := func(p string) string { return strings.TrimPrefix(p, subPrefix) }

	opts := index.SearchOpts{
		Literal:   index.ExtractLiteral(req.Pattern, req.Fixed),
		FilesOnly: req.Files || req.Count,
		Limit:     req.Limit,
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
	res := s.idx.Search(re, opts)

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
			if send(proto.Event{Type: "match", File: strip(m.Path), Line: m.Line, Text: m.Text}) != nil {
				return
			}
		}
	}
	total := 0
	for _, c := range res.FileCounts {
		total += c
	}
	_ = send(proto.Event{Type: "done", Matches: total, FileHits: len(res.FileCounts),
		DurMS: time.Since(start).Milliseconds(), Truncated: res.Truncated})
}

// streamUntilReady emits progress events while the initial scan runs.
// Returns false if the client disconnected.
func streamUntilReady(s *RootStore, send func(proto.Event) error) bool {
	if s.State() == StateReady {
		return true
	}
	tick := time.NewTicker(progressInterval)
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
	sv := NewServer(cacheDir)
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
