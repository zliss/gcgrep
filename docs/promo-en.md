# I built an indexed grep for AI coding agents — 5ms warm searches on the kubernetes repo, 50× faster than grep on Windows 11

> Repo: https://github.com/zliss/gcgrep (MIT, single static binary, zero config)

## The itch

AI coding agents (Claude Code, Cursor, etc.) grep *constantly* — find the
definition, find the callers, edit, grep again to verify. Dozens of
full-tree scans per task. On Windows 11 this is brutal: NTFS open overhead
plus Defender real-time scanning of every file read turns each search into
seconds of CPU burn.

The root cause: grep re-walks the entire tree every time, even though 99%
of the code hasn't changed. IDEs solved this decades ago with indexes —
but an AI agent can't call IntelliJ's index. It can only run CLI tools.

So I put an IDE-style index behind a grep-compatible CLI.

## Numbers (kubernetes/kubernetes, 30,482 files, measured)

| | macOS | Windows 11 |
|---|---|---|
| `grep -rn` / `findstr /s` | 260 ms | 1.8 s |
| PowerShell `Select-String` | — | 9.4 s |
| **gcgrep warm query** | **5 ms** | **37 ms** |
| one-time cold index | 8 s | 55 s |

Plus IDE-style symbol search for Go, Java, Python, TypeScript:

```text
$ gcgrep def NewSchedulerCommand ./kubernetes
cmd/kube-scheduler/app/server.go:93: [func NewSchedulerCommand] func NewSchedulerCommand(...)

$ gcgrep refs NewSchedulerCommand ./kubernetes   # all call sites in 6ms
$ gcgrep symbols pkg/scheduler/scheduler.go      # file outline
```

## How it works

A resident daemon builds a trigram index (same idea as ripgrep's literal
optimization and Google Code Search / zoekt) plus per-file symbol tables on
first search, keeps file contents in memory, and applies fsnotify changes
incrementally. The CLI talks to it over a unix socket / Windows named pipe
— no TCP ports. Indexes persist; a restart does a stat-only reconcile that
also catches changes made while the daemon was down.

### The detail that matters for agents: read-after-write consistency

An agent's loop is *edit file → immediately search to verify*. Every cached
search tool has a silent-staleness window here: the watch event hasn't
landed yet, you get the old content, and nothing tells you. gcgrep borrows
watchman's cookie-file trick — each query drops a cookie into the watched
root and waits for its event to come out of the OS queue (proving all
earlier writes were delivered) before answering. Measured cost: ~1 ms.
That guarantee is what makes it safe to wire into an agent as the default
search tool.

### Honest limits

- `refs` is a syntax-level candidate list: it can't distinguish overloads
  or same-named methods of unrelated types — that's type resolution,
  i.e. LSP territory. For agents, high-recall candidates are exactly what
  they want anyway; they read the context themselves.
- Go symbols use the real stdlib parser; Java/Python/TS are ctags-grade
  heuristics (comment/string-stripped lexical analysis).
- Contents live in RAM: ~1.5× source size (kubernetes ≈ 700 MB).

## Things I learned the hard way

1. Go's regexp with `(?i)` is shockingly slow: a case-insensitive literal
   query took 1.1 s; a hand-rolled ASCII case-fold + `bytes.Index` fast
   path took 66 ms.
2. On macOS, `cp` over a running binary gets new executions SIGKILLed by
   the code-signing cache. `rm` first.
3. fsnotify event buffers overflow (especially ReadDirectoryChangesW on
   Windows during a big `git checkout`). Without an overflow → full
   reconcile fallback, the index rots silently.
4. tree-sitter is great but cgo would have killed single-machine
   cross-compilation and the static single binary. The ctags-style
   pure-Go approach turned out to be the right trade.

## For your agent

```markdown
- Prefer `gcgrep` over grep/rg for code search (fall back to grep if absent).
  Same output format and exit codes.
  text: gcgrep PATTERN [DIR] · defs: gcgrep def NAME · calls: gcgrep refs NAME
  Searching right after editing files is safe (read-after-write consistent).
```

Grab a binary from [Releases](https://github.com/zliss/gcgrep/releases),
drop it on PATH, done — the daemon auto-starts on first use.

Tested on macOS, Windows 11 and Linux: index correctness, gitignore,
live-watch, persistence/reconcile, a no-sleep write-then-search hammer
test, and per-language symbol extraction suites. Adding a language is one
extractor file + tests — PRs welcome.
