// Package proto defines the JSON-lines protocol between client and daemon.
// Each request and each response event is one JSON object per line.
package proto

const Version = "0.1.0"

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
}

// Event.Type values: "progress", "match", "filecount", "done",
// "status", "error".
type Event struct {
	Type string `json:"type"`

	// progress
	Indexed int    `json:"indexed,omitempty"`
	Total   int    `json:"total,omitempty"`
	Stage   string `json:"stage,omitempty"` // "indexing" | "reconciling"

	// match / filecount
	File  string `json:"file,omitempty"`
	Line  int    `json:"line,omitempty"`
	Text  string `json:"text,omitempty"`
	Count int    `json:"count,omitempty"`

	// done
	Matches   int   `json:"matches,omitempty"`
	FileHits  int   `json:"fileHits,omitempty"`
	DurMS     int64 `json:"durMs,omitempty"`
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
}
