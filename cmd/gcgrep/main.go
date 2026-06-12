// Command gcgrep is an indexed, grep-like code search tool. The first
// search of a directory builds a trigram index held by a resident daemon
// (auto-started); file changes are watched and applied incrementally, so
// subsequent searches answer from memory in milliseconds.
//
// Exit codes follow grep: 0 = matches found, 1 = no matches, 2 = error.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"bytes"

	"github.com/zliss/gcgrep/internal/conf"
	"github.com/zliss/gcgrep/internal/daemon"
	"github.com/zliss/gcgrep/internal/index"
	"github.com/zliss/gcgrep/internal/ipc"
	"github.com/zliss/gcgrep/internal/proto"
)

var cfg = conf.Load()

// lineReader lazily reads files to render long-line matches the daemon
// sent as location-only. Caches the most recent file: matches arrive
// sorted by path, so consecutive hits in one file cost a single read.
type lineReader struct {
	path  string
	lines [][]byte
}

func (lr *lineReader) render(absPath string, line, col, lineLen, maxCols int) string {
	if lr.path != absPath {
		content, err := os.ReadFile(absPath)
		if err != nil {
			return fmt.Sprintf("[line unavailable: %s, %d bytes]", err, lineLen)
		}
		lr.path = absPath
		lr.lines = bytes.Split(content, []byte("\n"))
	}
	if line < 1 || line > len(lr.lines) || len(lr.lines[line-1]) != lineLen {
		// file changed between daemon answer and our read: do not show
		// content that may no longer correspond to the reported line
		return fmt.Sprintf("[line changed on disk, %d bytes at match time]", lineLen)
	}
	return index.TruncateWindow(lr.lines[line-1], col, maxCols)
}

func main() {
	os.Exit(run())
}

func run() int {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "def", "refs", "symbols":
			return cmdSymbol(args[0], args[1:])
		case "daemon":
			return cmdDaemon()
		case "stop":
			return cmdSimple("stop")
		case "status":
			return cmdSimple("status")
		case "version", "--version", "-V":
			fmt.Println("gcgrep " + proto.Version)
			return 0
		case "help", "--help", "-h":
			usage()
			return 0
		}
	}
	return cmdSearch(args)
}

func usage() {
	fmt.Print(`gcgrep - indexed grep for code (daemon-backed, incremental)

Usage:
  gcgrep [options] PATTERN [PATH]   search PATH (default .) for regex PATTERN
  gcgrep def [-i] NAME [PATH]       find symbol definitions (class/func/method)
  gcgrep refs NAME [PATH]           find candidate references/calls by name
  gcgrep symbols FILE               list all definitions in FILE
  gcgrep status                     show daemon state and indexed roots
  gcgrep stop                       stop the daemon (indexes are persisted)
  gcgrep daemon                     run the daemon in the foreground

Symbol support: Go, Java, Python, TypeScript/JavaScript. Definitions are
parser/ctags-grade; refs are syntax-level candidates by name (overloads
and same-named members of other types are not distinguished).

Options:
  -i            case-insensitive search
  -F            treat PATTERN as a fixed string, not a regex
  -l            print only names of files with matches
  -c            print match counts per file
  -g GLOB       only search files matching GLOB (repeatable)
  --json        output one JSON object per event (machine-readable)
  --limit N     stop after N matching lines (default 2000, 0 = no limit)
  --no-sync     skip the read-after-write barrier (faster, may miss writes
                made in the last fraction of a second)
  --max-columns N  cap displayed match lines to a window of N bytes
                around the hit (default unlimited, like grep/rg; long
                lines transfer as location-only and render from disk)

Tunables via env (daemon logs effective overrides): GCGREP_MAX_FILESIZE_MB
(default 2), GCGREP_MAX_INDEX_MB (0=unlimited), GCGREP_BARRIER_TIMEOUT_MS,
GCGREP_DEBOUNCE_MS, GCGREP_SAVE_DELAY_MS, GCGREP_WORKERS, GCGREP_LIMIT,
GCGREP_MAX_COLUMNS, GCGREP_SPAWN_TIMEOUT_MS, GCGREP_DIAL_TIMEOUT_MS.

First search of a directory builds the index (progress on stderr); later
searches and file changes are served from the live index.
`)
}

// ---- daemon subcommand ----

func cmdDaemon() int {
	cacheDir, err := ipc.CacheDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "gcgrep:", err)
		return 2
	}
	l, err := ipc.Listen()
	if err != nil {
		// a live daemon already owns the endpoint: not an error for
		// the spawn race, but report it for manual invocations
		fmt.Fprintln(os.Stderr, "gcgrep daemon:", err)
		return 2
	}
	if err := daemon.Run(l, cacheDir, filepath.Join(cacheDir, "daemon.log")); err != nil {
		return 2
	}
	return 0
}

// ---- client ----

type cliOpts struct {
	req     proto.Request
	jsonOut bool
	pathArg string
}

func parseArgs(args []string) (cliOpts, error) {
	fs := flag.NewFlagSet("gcgrep", flag.ContinueOnError)
	fs.Usage = usage
	o := cliOpts{}
	var globs multiFlag
	fs.BoolVar(&o.req.NoCase, "i", false, "")
	fs.BoolVar(&o.req.Fixed, "F", false, "")
	fs.BoolVar(&o.req.Files, "l", false, "")
	fs.BoolVar(&o.req.Count, "c", false, "")
	fs.Var(&globs, "g", "")
	fs.BoolVar(&o.jsonOut, "json", false, "")
	fs.IntVar(&o.req.Limit, "limit", cfg.Limit, "")
	fs.BoolVar(&o.req.NoSync, "no-sync", false, "")
	fs.IntVar(&o.req.MaxColumns, "max-columns", 0, "")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	rest := fs.Args()
	if len(rest) < 1 || len(rest) > 2 {
		usage()
		return o, fmt.Errorf("expected PATTERN [PATH]")
	}
	o.req.Op = "search"
	o.req.Pattern = rest[0]
	o.req.Globs = globs
	o.pathArg = "."
	if len(rest) == 2 {
		o.pathArg = rest[1]
	}
	abs, err := filepath.Abs(o.pathArg)
	if err != nil {
		return o, err
	}
	// resolve symlinks so the same tree always maps to the same root key
	if resolved, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		abs = resolved
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return o, err
	}
	if !fi.IsDir() {
		return o, fmt.Errorf("PATH must be a directory: %s", o.pathArg)
	}
	o.req.Root = abs
	return o, nil
}

type multiFlag []string

func (m *multiFlag) String() string     { return fmt.Sprint([]string(*m)) }
func (m *multiFlag) Set(s string) error { *m = append(*m, s); return nil }

// cmdSymbol handles def/refs/symbols. For symbols the positional argument
// is a file; its parent directory becomes the search root.
func cmdSymbol(op string, args []string) int {
	var o cliOpts
	var err error
	if op == "symbols" {
		o, err = parseSymbolsFile(args)
	} else {
		o, err = parseArgs(args)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gcgrep:", err)
		return 2
	}
	o.req.Op = op
	conn, cerr := connectOrSpawn()
	if cerr != nil {
		fmt.Fprintln(os.Stderr, "gcgrep:", cerr)
		return 2
	}
	defer conn.Close()
	return streamSearch(conn, o)
}

func parseSymbolsFile(args []string) (cliOpts, error) {
	o := cliOpts{}
	if len(args) != 1 {
		return o, fmt.Errorf("expected FILE")
	}
	abs, err := filepath.Abs(args[0])
	if err != nil {
		return o, err
	}
	if resolved, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		abs = resolved
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return o, err
	}
	if fi.IsDir() {
		return o, fmt.Errorf("FILE must be a file: %s", args[0])
	}
	o.req.Root = filepath.Dir(abs)
	o.req.Pattern = filepath.Base(abs)
	o.req.Limit = 0
	o.pathArg = "."
	return o, nil
}

func cmdSearch(args []string) int {
	o, err := parseArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gcgrep:", err)
		return 2
	}
	conn, err := connectOrSpawn()
	if err != nil {
		fmt.Fprintln(os.Stderr, "gcgrep:", err)
		return 2
	}
	defer conn.Close()
	return streamSearch(conn, o)
}

func streamSearch(conn net.Conn, o cliOpts) int {
	if err := json.NewEncoder(conn).Encode(&o.req); err != nil {
		fmt.Fprintln(os.Stderr, "gcgrep:", err)
		return 2
	}
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	dec := json.NewDecoder(bufio.NewReader(conn))
	matched := false
	progressShown := false
	var lr lineReader
	displayCols := o.req.MaxColumns
	if displayCols == 0 {
		displayCols = cfg.MaxColumns // 0 = unlimited, like grep/rg
	}
	// prefix printed before each file path, mirroring the PATH argument
	prefix := ""
	if o.pathArg != "." {
		prefix = filepath.ToSlash(filepath.Clean(o.pathArg)) + "/"
	}
	for {
		var ev proto.Event
		if err := dec.Decode(&ev); err != nil {
			fmt.Fprintln(os.Stderr, "gcgrep: connection lost:", err)
			return 2
		}
		if o.jsonOut {
			b, _ := json.Marshal(&ev)
			fmt.Fprintln(out, string(b))
			if ev.Type == "done" || ev.Type == "error" {
				if ev.Type == "error" {
					return 2
				}
				if ev.Matches > 0 {
					return 0
				}
				return 1
			}
			continue
		}
		switch ev.Type {
		case "progress":
			fmt.Fprintf(os.Stderr, "\rgcgrep: %s %d/%d files...", ev.Stage, ev.Indexed, ev.Total)
			progressShown = true
		case "match":
			clearProgress(&progressShown)
			if ev.Text == "" && ev.LineLen > 0 {
				// long line: daemon sent location only; render from disk
				ev.Text = lr.render(filepath.Join(o.req.Root, filepath.FromSlash(ev.File)), ev.Line, ev.Col, ev.LineLen, displayCols)
			}
			if ev.Kind != "" {
				qual := ev.Kind
				if ev.Container != "" {
					qual += " " + ev.Container + "." + ev.Name
				} else if ev.Name != "" && ev.Kind != "call" && ev.Kind != "ref" {
					qual += " " + ev.Name
				}
				fmt.Fprintf(out, "%s%s:%d: [%s] %s\n", prefix, ev.File, ev.Line, qual, strings.TrimSpace(ev.Text))
			} else {
				fmt.Fprintf(out, "%s%s:%d:%s\n", prefix, ev.File, ev.Line, ev.Text)
			}
			matched = true
		case "filecount":
			clearProgress(&progressShown)
			if o.req.Count {
				fmt.Fprintf(out, "%s%s:%d\n", prefix, ev.File, ev.Count)
			} else {
				fmt.Fprintf(out, "%s%s\n", prefix, ev.File)
			}
			matched = true
		case "done":
			clearProgress(&progressShown)
			if ev.Truncated {
				fmt.Fprintf(os.Stderr, "gcgrep: output truncated at %d lines (use --limit 0 for all)\n", o.req.Limit)
			}
			if matched || ev.Matches > 0 {
				return 0
			}
			return 1
		case "error":
			clearProgress(&progressShown)
			fmt.Fprintln(os.Stderr, "gcgrep:", ev.Msg)
			return 2
		}
	}
}

func clearProgress(shown *bool) {
	if *shown {
		fmt.Fprint(os.Stderr, "\r\033[K")
		*shown = false
	}
}

func cmdSimple(op string) int {
	conn, err := ipc.Dial(cfg.DialTimeout)
	if err != nil {
		if op == "stop" {
			fmt.Println("gcgrep: daemon not running")
			return 0
		}
		fmt.Fprintln(os.Stderr, "gcgrep: daemon not running")
		return 1
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(&proto.Request{Op: op}); err != nil {
		fmt.Fprintln(os.Stderr, "gcgrep:", err)
		return 2
	}
	var ev proto.Event
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&ev); err != nil {
		fmt.Fprintln(os.Stderr, "gcgrep:", err)
		return 2
	}
	switch ev.Type {
	case "status":
		fmt.Printf("daemon running, pid %d\n", ev.PID)
		for _, r := range ev.Roots {
			extra := ""
			if r.SkippedLarge > 0 {
				extra += fmt.Sprintf("  skipped %d files > GCGREP_MAX_FILESIZE_MB", r.SkippedLarge)
			}
			if r.SkippedBudget > 0 {
				extra += fmt.Sprintf("  skipped %d files over GCGREP_MAX_INDEX_MB", r.SkippedBudget)
			}
			if r.SkippedBinary > 0 {
				extra += fmt.Sprintf("  skipped %d binary files", r.SkippedBinary)
			}
			if r.SkippedError > 0 {
				extra += fmt.Sprintf("  skipped %d unreadable files", r.SkippedError)
			}
			fmt.Printf("  %s  [%s]  %d files, %dMB%s\n", r.Root, r.State, r.Files, r.SizeMB, extra)
		}
		if len(ev.Roots) == 0 {
			fmt.Println("  (no roots indexed yet)")
		}
	case "done":
		fmt.Println("gcgrep: daemon stopped")
	case "error":
		fmt.Fprintln(os.Stderr, "gcgrep:", ev.Msg)
		return 2
	}
	return 0
}

// connectOrSpawn dials the daemon, starting it on demand. Concurrent
// spawns are harmless: the loser fails to bind and exits.
func connectOrSpawn() (net.Conn, error) {
	if conn, err := ipc.Dial(cfg.DialTimeout); err == nil {
		return conn, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if err := spawnDaemon(exe); err != nil {
		return nil, fmt.Errorf("starting daemon: %w", err)
	}
	deadline := time.Now().Add(cfg.SpawnTimeout)
	for time.Now().Before(deadline) {
		if conn, derr := ipc.Dial(cfg.DialTimeout); derr == nil {
			return conn, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, fmt.Errorf("daemon did not come up within %s (see daemon.log in cache dir)", cfg.SpawnTimeout)
}
