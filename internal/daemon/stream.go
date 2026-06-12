package daemon

import "sort"

// The stream set is a manifest of files that are searchable but not held
// in memory: files larger than the index threshold, binary files, and
// files beyond the per-root byte budget. The daemon never scans them; at
// query time it emits one "streamfile" event per qualifying entry and the
// CLIENT scans those files from disk — the same trade rg makes for every
// file, applied only to the long tail. Binary detection also happens on
// the client (NUL probe), since the daemon does not read these files.

type streamEntry struct {
	size    int64
	mtimeNS int64
}

func (s *RootStore) streamPut(rel string, size, mtimeNS int64) {
	s.streamMu.Lock()
	s.stream[rel] = streamEntry{size: size, mtimeNS: mtimeNS}
	s.streamMu.Unlock()
}

// streamGet is used by reconcile to skip re-routing unchanged entries.
func (s *RootStore) streamGet(rel string) (streamEntry, bool) {
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	e, ok := s.stream[rel]
	return e, ok
}

func (s *RootStore) streamDelete(rel string) {
	s.streamMu.Lock()
	delete(s.stream, rel)
	s.streamMu.Unlock()
}

func (s *RootStore) streamDeletePrefix(dir string) {
	prefix := dir + "/"
	s.streamMu.Lock()
	for p := range s.stream {
		if p == dir || len(p) > len(prefix) && p[:len(prefix)] == prefix {
			delete(s.stream, p)
		}
	}
	s.streamMu.Unlock()
}

// streamRetain drops stream entries not present on disk (reconcile).
func (s *RootStore) streamRetain(onDisk map[string]struct{}) {
	s.streamMu.Lock()
	for p := range s.stream {
		if _, ok := onDisk[p]; !ok {
			delete(s.stream, p)
		}
	}
	s.streamMu.Unlock()
}

func (s *RootStore) streamCount() int {
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	return len(s.stream)
}

// StreamFile is one manifest entry handed to the server for emission.
type StreamFile struct {
	Rel  string
	Size int64
}

// StreamList returns manifest entries passing the query's size and path
// filters, sorted by path for deterministic client output.
func (s *RootStore) StreamList(maxFilesize int64, pathOK func(rel string) bool) []StreamFile {
	s.streamMu.Lock()
	out := make([]StreamFile, 0, len(s.stream))
	for rel, e := range s.stream {
		if maxFilesize > 0 && e.size > maxFilesize {
			continue
		}
		if pathOK != nil && !pathOK(rel) {
			continue
		}
		out = append(out, StreamFile{Rel: rel, Size: e.size})
	}
	s.streamMu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Rel < out[j].Rel })
	return out
}
