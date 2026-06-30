// Package conf centralizes tunables that were previously hardcoded.
// Every knob reads a GCGREP_* environment variable (daemon and client
// both load it at startup; the daemon logs effective values). Defaults
// preserve prior behavior except MaxColumns, which now defaults to
// unlimited to match grep/rg.
package conf

import (
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	MaxFileSizeMB    int           // GCGREP_MAX_FILESIZE_MB: files larger than this are not indexed
	MaxIndexMB       int           // GCGREP_MAX_INDEX_MB: per-root content budget, 0 = unlimited
	BarrierTimeout   time.Duration // GCGREP_BARRIER_TIMEOUT_MS: read-after-write sync upper bound
	Debounce         time.Duration // GCGREP_DEBOUNCE_MS: watcher event coalescing window
	SaveDelay        time.Duration // GCGREP_SAVE_DELAY_MS: index persistence debounce
	Workers          int           // GCGREP_WORKERS: parallel indexing workers
	SpawnTimeout     time.Duration // GCGREP_SPAWN_TIMEOUT_MS: client wait for daemon startup
	DialTimeout      time.Duration // GCGREP_DIAL_TIMEOUT_MS: client connect timeout
	MaxColumns       int           // GCGREP_MAX_COLUMNS: default match-line display cap, 0 = unlimited
	Limit            int           // GCGREP_LIMIT: default match cap, 0 = unlimited
	ProgressInterval time.Duration // GCGREP_PROGRESS_INTERVAL_MS: indexing progress frequency
	Priority         string        // GCGREP_PRIORITY: "low" (default) or "normal" daemon process priority
	Engine           string        // GCGREP_ENGINE: "auto" (default), "mem", or "disk"
	DiskEngineMB     int           // GCGREP_DISK_ENGINE_MB: auto-switch threshold (default 512)
	RebuildInterval  time.Duration // GCGREP_REBUILD_INTERVAL_MS: shard rebuild cycle (default 20s)
	ShardTargetMB    int           // GCGREP_SHARD_TARGET_MB: target shard source size (default 80)
	ShardMinMB       int           // GCGREP_SHARD_MIN_MB: merge shards below this (default 32)
	ShardMaxMB       int           // GCGREP_SHARD_MAX_MB: split shards above this (default 128)
	RebuildThreshold int           // GCGREP_REBUILD_THRESHOLD: min dirty files to trigger shard rebuild (default 20)
	SearchWorkers    int           // GCGREP_SEARCH_WORKERS: parallel search goroutines (default 2)
	MemoryLimitMB    int           // GCGREP_MEMORY_LIMIT_MB: Go soft memory limit for disk engine daemon (default 100)
	ShardInlineKB    int           // GCGREP_SHARD_INLINE_KB: inline file content in shard for files ≤ this size (Windows only, default 0)
	Exclude          []string      // GCGREP_EXCLUDE: comma-separated glob patterns excluded from indexing and search
	Gitignore        bool          // GCGREP_GITIGNORE: honor .gitignore files during indexing (default false)
}

func Default() Config {
	return Config{
		MaxFileSizeMB:    2,
		MaxIndexMB:       256,
		BarrierTimeout:   2 * time.Second,
		Debounce:         200 * time.Millisecond,
		SaveDelay:        30 * time.Second,
		Workers:          defaultWorkers(),
		SpawnTimeout:     5 * time.Second,
		DialTimeout:      500 * time.Millisecond,
		MaxColumns:       0,
		Limit:            2000,
		ProgressInterval: 300 * time.Millisecond,
		Priority:         "low",
		Engine:           "auto",
		DiskEngineMB:     10,
		RebuildInterval:  20 * time.Second,
		ShardTargetMB:    80,
		ShardMinMB:       32,
		ShardMaxMB:       128,
		RebuildThreshold: 20,
		SearchWorkers:    2,
		MemoryLimitMB:    100,
		ShardInlineKB:    0,
	}
}

// defaultWorkers keeps indexing from saturating the machine: a background
// helper should never compete with the user's build/IDE for every core.
func defaultWorkers() int {
	const maxDefaultWorkers = 8
	w := runtime.NumCPU() / 2
	if w > maxDefaultWorkers {
		w = maxDefaultWorkers
	}
	if w < 1 {
		w = 1
	}
	return w
}

// Load returns defaults overridden by GCGREP_* environment variables.
func Load() Config {
	c := Default()
	intVar(&c.MaxFileSizeMB, "GCGREP_MAX_FILESIZE_MB")
	intVar(&c.MaxIndexMB, "GCGREP_MAX_INDEX_MB")
	msVar(&c.BarrierTimeout, "GCGREP_BARRIER_TIMEOUT_MS")
	msVar(&c.Debounce, "GCGREP_DEBOUNCE_MS")
	msVar(&c.SaveDelay, "GCGREP_SAVE_DELAY_MS")
	intVar(&c.Workers, "GCGREP_WORKERS")
	msVar(&c.SpawnTimeout, "GCGREP_SPAWN_TIMEOUT_MS")
	msVar(&c.DialTimeout, "GCGREP_DIAL_TIMEOUT_MS")
	intVar(&c.MaxColumns, "GCGREP_MAX_COLUMNS")
	intVar(&c.Limit, "GCGREP_LIMIT")
	msVar(&c.ProgressInterval, "GCGREP_PROGRESS_INTERVAL_MS")
	if v, ok := os.LookupEnv("GCGREP_PRIORITY"); ok && (v == "low" || v == "normal") {
		c.Priority = v
	}
	if v, ok := os.LookupEnv("GCGREP_ENGINE"); ok && (v == "auto" || v == "mem" || v == "disk") {
		c.Engine = v
	}
	intVar(&c.DiskEngineMB, "GCGREP_DISK_ENGINE_MB")
	msVar(&c.RebuildInterval, "GCGREP_REBUILD_INTERVAL_MS")
	intVar(&c.ShardTargetMB, "GCGREP_SHARD_TARGET_MB")
	intVar(&c.ShardMinMB, "GCGREP_SHARD_MIN_MB")
	intVar(&c.ShardMaxMB, "GCGREP_SHARD_MAX_MB")
	intVar(&c.RebuildThreshold, "GCGREP_REBUILD_THRESHOLD")
	intVar(&c.SearchWorkers, "GCGREP_SEARCH_WORKERS")
	intVar(&c.MemoryLimitMB, "GCGREP_MEMORY_LIMIT_MB")
	intVar(&c.ShardInlineKB, "GCGREP_SHARD_INLINE_KB")
	if v, ok := os.LookupEnv("GCGREP_GITIGNORE"); ok && (v == "1" || v == "true") {
		c.Gitignore = true
	}
	if v, ok := os.LookupEnv("GCGREP_EXCLUDE"); ok && v != "" {
		c.Exclude = strings.Split(v, ",")
	}
	if c.Workers < 1 {
		c.Workers = 1
	}
	if c.SearchWorkers < 1 {
		c.SearchWorkers = 1
	}
	if c.RebuildThreshold < 1 {
		c.RebuildThreshold = 1
	}
	return c
}

func (c Config) MaxFileSize() int64 { return int64(c.MaxFileSizeMB) << 20 }

// Overrides lists the GCGREP_* variables present in the environment,
// for daemon startup logging.
func Overrides() []string {
	keys := []string{"GCGREP_MAX_FILESIZE_MB", "GCGREP_MAX_INDEX_MB", "GCGREP_BARRIER_TIMEOUT_MS",
		"GCGREP_DEBOUNCE_MS", "GCGREP_SAVE_DELAY_MS", "GCGREP_WORKERS", "GCGREP_SPAWN_TIMEOUT_MS",
		"GCGREP_DIAL_TIMEOUT_MS", "GCGREP_MAX_COLUMNS", "GCGREP_LIMIT", "GCGREP_PROGRESS_INTERVAL_MS",
		"GCGREP_PRIORITY", "GCGREP_ENGINE", "GCGREP_DISK_ENGINE_MB", "GCGREP_REBUILD_INTERVAL_MS",
		"GCGREP_SHARD_TARGET_MB", "GCGREP_SHARD_MIN_MB", "GCGREP_SHARD_MAX_MB",
		"GCGREP_REBUILD_THRESHOLD", "GCGREP_SEARCH_WORKERS", "GCGREP_MEMORY_LIMIT_MB",
		"GCGREP_SHARD_INLINE_KB", "GCGREP_EXCLUDE", "GCGREP_GITIGNORE"}
	var out []string
	for _, k := range keys {
		if v, ok := os.LookupEnv(k); ok {
			out = append(out, fmt.Sprintf("%s=%s", k, v))
		}
	}
	sort.Strings(out)
	return out
}

func intVar(dst *int, key string) {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			*dst = n
		}
	}
}

func msVar(dst *time.Duration, key string) {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			*dst = time.Duration(n) * time.Millisecond
		}
	}
}
