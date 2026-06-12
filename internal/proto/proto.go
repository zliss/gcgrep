// Package proto defines the JSON-lines protocol between client and daemon.
// Each request and each response event is one JSON object per line.
package proto

const Version = "0.4.0"

type Request struct {
	Op      string   `json:"op"` // "search" | "status" | "stop"
	Root    string   `json:"root,omitempty"`
	Pattern string   `json:"pattern,omitempty"`
	Fixed   bool     `json:"fixed,omitempty"`
	NoCase  bool     `json:"nocase,omitempty"`
	Files   bool     `json:"files,omitempty"`
	Count   bool     `json:"count,omitempty"`
	Globs   []string `json:"globs,omitempty"`
	Limit   int      `json:"limit,omitempty"`
	NoSync  bool     `json:"nosync,omitempty"` // skip the read-after-write barrier
	// MaxColumns truncates emitted match lines (0 = daemon default 4096,
	// -1 = unlimited); protects pipes and AI token budgets from minified
	// single-line JSON/XML.
	MaxColumns int `json:"maxcols,omitempty"`
}

// Event.Type values: "progress", "match", "filecount", "done",
// "status", "error".
type Event struct {
	Type string `json:"type"`

	// progress
	Indexed int    `json:"indexed,omitempty"`
	Total   int    `json:"total,omitempty"`
	Stage   string `json:"stage,omitempty"` // "indexing" | "reconciling"

	// match / filecount. For lines longer than max-columns the daemon
	// omits Text and sends Col (match offset in line) + LineLen instead;
	// the client re-reads the file to render a window around the hit.
	File    string `json:"file,omitempty"`
	Line    int    `json:"line,omitempty"`
	Text    string `json:"text,omitempty"`
	Count   int    `json:"count,omitempty"`
	Col     int    `json:"col,omitempty"`
	LineLen int    `json:"lineLen,omitempty"`

	// symbol matches (def / refs / symbols)
	Name      string `json:"name,omitempty"`
	Kind      string `json:"kind,omitempty"` // class/struct/interface/enum/type/func/method | call/ref
	Container string `json:"container,omitempty"`

	// done
	Matches   int   `json:"matches,omitempty"`
	FileHits  int   `json:"fileHits,omitempty"`
	DurMS     int64 `json:"durMs,omitempty"`
	BarrierMS int64 `json:"barrierMs,omitempty"` // read-after-write sync portion
	SearchMS  int64 `json:"searchMs,omitempty"`  // pure index search portion
	Truncated bool  `json:"truncated,omitempty"`

	// status
	Roots []RootStatus `json:"roots,omitempty"`
	PID   int          `json:"pid,omitempty"`

	// error
	Msg string `json:"msg,omitempty"`
}

type RootStatus struct {
	Root  string `json:"root"`
	State string `json:"state"` // "indexing" | "reconciling" | "ready"
	Files int    `json:"files"`
	// observability: what was NOT indexed and why
	SizeMB        int `json:"sizeMb"`
	SkippedLarge  int `json:"skippedLarge,omitempty"`  // over GCGREP_MAX_FILESIZE_MB
	SkippedBudget int `json:"skippedBudget,omitempty"` // over GCGREP_MAX_INDEX_MB
	SkippedBinary int `json:"skippedBinary,omitempty"` // NUL byte in first 8KB (incl UTF-16)
	SkippedError  int `json:"skippedError,omitempty"`  // unreadable: permissions etc.
}
