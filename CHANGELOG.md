# Changelog

## v1.0.0 (2026-06-13)

Disk shard engine for large codebases — the daemon no longer needs to hold
all file contents in memory.

- **Disk shard engine**: when source size exceeds `GCGREP_DISK_ENGINE_MB`
  (default 512MB), the daemon builds immutable trigram shard files on disk
  instead of holding everything in memory. Queries intersect shard posting
  lists then verify by reading the original files. Symbol tables remain
  in memory (~1-3% of source size).
- **Dirty list**: file changes from the watcher are appended to a dirty
  list (instant, no I/O). Queries combine shard results (excluding dirty
  entries) with live disk scans of dirty files — write-after-read
  consistency is preserved with zero latency cost.
- **Background rebuild**: every `GCGREP_REBUILD_INTERVAL_MS` (default 20s),
  affected shards are rebuilt and atomically swapped in.
- `GCGREP_ENGINE=auto|mem|disk` forces engine selection.
- `gcgrep status` shows `[disk]` for disk-engine roots and the engine type
  in the JSON status.

Verified on a 1GB / 44k-file corpus (macOS arm64):
  - Index size: 37MB (3.7% of source)
  - Warm p50: 29ms, p99: 32ms
  - Daemon RSS: 416MB
  - Write-then-read: 25/25 rounds correct

## v0.5.0 (2026-06-12)

ripgrep-alignment release: nothing under a root is silently unsearchable
anymore.

- **Stream set**: files the daemon does not index (over
  `GCGREP_MAX_FILESIZE_MB`, binary, over the index budget) are now
  tracked in a manifest and **searched anyway** — the daemon announces
  them per query and the client scans them from disk, rg-style. Applies
  to all modes (`-l`, `-c`, `--json`). `gcgrep status` shows the count.
- `--hidden`: dot-files/dirs are now skipped by default (rg parity,
  query-time filter — no reindex when toggling) and included with the
  flag.
- `-a/--text`: search binary files as text (client-side NUL probe).
- `-L/--follow`: follow symlinked files/dirs with cycle protection; a
  follow variant keeps its own index per root.
- `--max-filesize SIZE` (K/M/G suffixes): exclude large files per query.
- UTF-16 files (BOM) are transcoded and indexed as UTF-8 — previously
  misclassified as binary.
- Resource restraint: indexing workers default to min(cores/2, 8); the
  daemon runs at low OS priority (`nice +10` / `BELOW_NORMAL`),
  `GCGREP_PRIORITY=normal` opts out.
- Version handshake: client warns when the daemon is an older version
  (an old daemon silently ignores new flags).
- Fixed a race where parallel indexing workers could overshoot
  `GCGREP_MAX_INDEX_MB`.


## v0.4.0 (2026-06-12)

- All hardcoded tunables are now GCGREP_* environment variables:
  max file size, per-root index byte budget (new), barrier timeout,
  watcher debounce, save delay, worker count, default limit/max-columns,
  client timeouts. Daemon logs effective overrides.
- `--max-columns` default changed to UNLIMITED (grep/rg parity). When
  set, long lines transfer as location-only events and the client
  renders a window centered on the hit from disk.
- `gcgrep status` now reports indexed size and counts of files skipped
  for size/budget reasons (previously silent).


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
