# Changelog

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
