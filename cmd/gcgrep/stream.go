package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/zliss/gcgrep/internal/index"
	"github.com/zliss/gcgrep/internal/proto"
)

// Client-side scanning of stream-set files. The daemon tracks files it
// does not hold in memory (too large / binary / over budget) and announces
// them as "streamfile" events; the client scans them from disk with the
// same matcher the daemon uses, so results are indistinguishable from
// indexed matches.

const (
	streamProbeBytes = 8192    // NUL probe window, same as the indexer
	streamBufBytes   = 1 << 20 // line-read buffer; lines may still grow past it
)

type streamScan struct {
	out         *bufio.Writer
	o           cliOpts
	prefix      string
	displayCols int
	m           index.Matcher

	limitLeft  int // remaining match-line budget; -1 = unlimited
	matches    int
	fileHits   int
	truncated  bool
	unreadable int
}

// scanStreams scans every announced stream file, emitting output in the
// same format (and mode: -l/-c/--json) as indexed matches. limitLeft is
// the match-line budget remaining after the daemon's indexed matches.
func scanStreams(files []string, out *bufio.Writer, o cliOpts, prefix string, displayCols, limitLeft int) *streamScan {
	ss := &streamScan{out: out, o: o, prefix: prefix, displayCols: displayCols, limitLeft: limitLeft}
	if len(files) == 0 {
		return ss
	}
	m, err := index.MatcherFor(o.req.Pattern, o.req.Fixed, o.req.NoCase)
	if err != nil {
		// the daemon validated the pattern before announcing stream files
		fmt.Fprintln(os.Stderr, "gcgrep: pattern recompile failed:", err)
		return ss
	}
	ss.m = m
	for _, rel := range files {
		if ss.limitLeft == 0 && !ss.filesOnly() {
			ss.truncated = true
			break
		}
		ss.scanFile(rel)
	}
	if ss.unreadable > 0 {
		fmt.Fprintf(os.Stderr, "gcgrep: %d stream file(s) unreadable, skipped\n", ss.unreadable)
	}
	return ss
}

func (ss *streamScan) filesOnly() bool { return ss.o.req.Files || ss.o.req.Count }

func (ss *streamScan) scanFile(rel string) {
	abs := filepath.Join(ss.o.req.Root, filepath.FromSlash(rel))
	f, err := os.Open(abs)
	if err != nil {
		ss.unreadable++
		return
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, streamBufBytes)
	head, _ := r.Peek(streamProbeBytes)
	if bytes.IndexByte(head, 0) >= 0 && !ss.o.req.Text {
		return // binary, and -a/--text not given (rg default)
	}
	lineNo, fileMatches := 0, 0
	for {
		line, rerr := r.ReadBytes('\n')
		if len(line) > 0 {
			lineNo++
			if n := len(line); line[n-1] == '\n' {
				line = line[:n-1]
			}
			if loc := ss.m.FindFirst(line); loc != nil {
				fileMatches++
				if !ss.filesOnly() {
					if ss.limitLeft == 0 {
						ss.truncated = true
						break
					}
					ss.emitMatch(rel, lineNo, line, loc[0])
					if ss.limitLeft > 0 {
						ss.limitLeft--
					}
				} else if ss.o.req.Files {
					break // -l: one match proves the file
				}
			}
		}
		if rerr != nil {
			if rerr != io.EOF {
				ss.unreadable++
			}
			break
		}
	}
	if fileMatches > 0 {
		ss.fileHits++
		ss.matches += fileMatches
		if ss.filesOnly() {
			ss.emitFileCount(rel, fileMatches)
		}
	}
}

func (ss *streamScan) emitMatch(rel string, line int, text []byte, col int) {
	rendered := index.TruncateWindow(text, col, ss.displayCols)
	if ss.o.jsonOut {
		ss.emitJSON(proto.Event{Type: "match", File: rel, Line: line, Text: rendered})
		return
	}
	fmt.Fprintf(ss.out, "%s%s:%d:%s\n", ss.prefix, rel, line, rendered)
}

func (ss *streamScan) emitFileCount(rel string, count int) {
	if ss.o.jsonOut {
		ev := proto.Event{Type: "filecount", File: rel}
		if ss.o.req.Count {
			ev.Count = count
		}
		ss.emitJSON(ev)
		return
	}
	if ss.o.req.Count {
		fmt.Fprintf(ss.out, "%s%s:%d\n", ss.prefix, rel, count)
	} else {
		fmt.Fprintf(ss.out, "%s%s\n", ss.prefix, rel)
	}
}

func (ss *streamScan) emitJSON(ev proto.Event) {
	b, _ := json.Marshal(&ev)
	fmt.Fprintln(ss.out, string(b))
}

// parseSize parses an rg-style size with optional K/M/G suffix (powers of
// 1024) into bytes.
func parseSize(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	mult := int64(1)
	switch s[len(s)-1] {
	case 'k', 'K':
		mult = 1 << 10
		s = s[:len(s)-1]
	case 'm', 'M':
		mult = 1 << 20
		s = s[:len(s)-1]
	case 'g', 'G':
		mult = 1 << 30
		s = s[:len(s)-1]
	}
	var n int64
	if len(s) == 0 {
		return 0, fmt.Errorf("invalid size")
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("invalid size %q (use e.g. 500K, 50M, 1G)", s)
		}
		n = n*10 + int64(s[i]-'0')
	}
	return n * mult, nil
}
