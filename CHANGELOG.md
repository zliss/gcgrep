# Changelog

## v0.3.0 (2026-06-12)

- BREAKING: `.gitignore` is no longer honored. gcgrep indexes everything
  except `.git`, binaries and >2MB files; explicit exclusions go in a
  root `.gcgrepignore` (gitignore syntax). Rationale: gitignored
  directories (e.g. Maven dependency sources) are often exactly what you
  want to search, and silent ignore semantics cost user trust.
- Match lines are truncated to a window around the hit (default 4096
  bytes, `--max-columns N`, -1 = unlimited) — minified single-line
  JSON/XML no longer floods output.
- Allocation-free case-insensitive literal search (no more full-content
  copy per file on `-i` queries).
- Match events no longer flush the pipe per line (high-hit queries on
  Windows named pipes were throttled by per-event flushes).
- `done` event now reports `barrierMs` and `searchMs` for diagnosing
  slow queries.


## v0.2.0 (2026-06-11)

- Symbol search: `def` / `refs` / `symbols` commands for Go, Java, Python,
  TypeScript/JavaScript (Go via stdlib parser, others ctags-grade).
- Read-after-write consistency: cookie-file barrier (watchman-style),
  `--no-sync` to opt out.
- Literal fast path: plain-literal and `-i` literal queries bypass the
  regex engine (`-i` on kubernetes: 1.1 s → 66 ms).
- Parallel index build/load/reconcile across CPU cores.
- Graceful shutdown: SIGTERM/SIGINT persist indexes; `stop` closes the
  listener before saving.

## v0.1.0 (2026-06-11)

- Daemon-backed trigram text search with grep-compatible CLI.
- Live file watching (fsnotify) with debounce and overflow reconcile.
- Index persistence + stat-only restart reconcile.
- IPC via unix socket / named pipe; no TCP ports.
- Platforms: macOS (arm64), Windows 11 (amd64); Linux builds and passes
  tests.
