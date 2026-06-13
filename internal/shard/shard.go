// Package shard implements an immutable, mmap-backed trigram index shard.
// Each shard covers a contiguous range of paths [PathMin, PathMax] and is
// loaded via OS mmap so data stays in the page cache, not on the Go heap.
//
// File format v3 (little-endian):
//
//	Header (64 bytes):
//	  magic      [8]byte  "GCSHARD1"
//	  version    uint32   3
//	  numFiles   uint32
//	  numTri     uint32
//	  numSyms    uint32   (total symbol entries across all files)
//	  fileTblOff uint64
//	  triIdxOff  uint64
//	  postOff    uint64
//	  symbolOff  uint64
//	  contentOff uint64   (0 = no inline content)
//
//	File table (at fileTblOff, 32 bytes per entry):
//	  pathOff    uint32   (into path blob)
//	  pathLen    uint16
//	  _          uint16
//	  size       int64
//	  mtimeNS    int64
//	  contentOff uint32   (into content blob, 0xFFFFFFFF = not inlined)
//	  contentLen uint32
//
//	Path blob (after file table):
//	  concatenated UTF-8 relative paths (sorted)
//
//	Trigram index (at triIdxOff):
//	  keys:    numTri × uint32 (sorted)
//	  offsets: numTri × uint32 (posting byte offset relative to postOff)
//
//	Postings (at postOff):
//	  per trigram: uvarint(numDocs), then numDocs uvarint-delta file IDs
//
//	Symbol table (at symbolOff):
//	  per file (in file-table order):
//	    uvarint(numDefs)
//	    per def: uvarint(nameLen), name, uvarint(kindLen), kind, uvarint(containerLen), container, uvarint(line)
//
//	Content blob (at contentOff, optional):
//	  concatenated file contents for inlined files
package shard

import (
	"encoding/binary"
	"fmt"
	"os"
	"sort"
	"sync"

	"github.com/zliss/gcgrep/internal/symbol"
)

var magic = [8]byte{'G', 'C', 'S', 'H', 'A', 'R', 'D', '1'}

const (
	headerSize   = 64
	fileEntryLen = 32
	formatVer    = 3
	noContent    = 0xFFFFFFFF
)

// FileMeta is the per-file metadata stored in a shard.
type FileMeta struct {
	Path    string
	Size    int64
	MtimeNS int64
}

// Shard is a loaded shard file descriptor. The actual data is mmap'd
// on demand during queries and munmap'd afterward, so the Go heap
// footprint is just the descriptor + trigram key table.
type Shard struct {
	path    string // filesystem path of .idx file
	size    int    // file size
	pathMin string // smallest path in this shard
	pathMax string // largest path in this shard

	// parsed from header (read once at open time via a small read)
	numFiles   uint32
	numTri     uint32
	numSyms    uint32
	fileTbl    int
	pathBlob   int
	triIdx     int
	postings   int
	symbolOff  int
	contentOff int // 0 = no inline content

	// triKeys is the sorted trigram key table, read into memory at Open
	// time. Allows skipping this shard entirely if the query's trigrams
	// are absent — no mmap needed.
	triKeys []uint32

	// mmap lifecycle: callers must call mmap()/munmap() around data access.
	// Multiple concurrent readers share the same mapping via refcount.
	mu      sync.Mutex
	data    []byte
	refs    int
	mapFile *os.File
}

// Open opens a shard file, reads its header and path range, but does
// NOT mmap the data. The caller must use Mmap/Munmap around queries.
func Open(path string) (*Shard, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := int(fi.Size())
	if size < headerSize {
		return nil, fmt.Errorf("shard too small: %d bytes", size)
	}

	var hdr [headerSize]byte
	if _, err := f.ReadAt(hdr[:], 0); err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	if [8]byte(hdr[:8]) != magic {
		return nil, fmt.Errorf("bad magic")
	}
	ver := binary.LittleEndian.Uint32(hdr[8:])
	if ver != formatVer {
		return nil, fmt.Errorf("unsupported version %d (want %d)", ver, formatVer)
	}

	s := &Shard{path: path, size: size}
	s.numFiles = binary.LittleEndian.Uint32(hdr[12:])
	s.numTri = binary.LittleEndian.Uint32(hdr[16:])
	s.numSyms = binary.LittleEndian.Uint32(hdr[20:])
	s.fileTbl = int(binary.LittleEndian.Uint64(hdr[24:]))
	s.triIdx = int(binary.LittleEndian.Uint64(hdr[32:]))
	s.postings = int(binary.LittleEndian.Uint64(hdr[40:]))
	s.symbolOff = int(binary.LittleEndian.Uint64(hdr[48:]))
	s.contentOff = int(binary.LittleEndian.Uint64(hdr[56:]))
	s.pathBlob = s.fileTbl + int(s.numFiles)*fileEntryLen

	// read pathMin and pathMax from the file table (first and last entry)
	if s.numFiles > 0 {
		s.pathMin, err = s.readPathAt(f, 0)
		if err != nil {
			return nil, fmt.Errorf("read pathMin: %w", err)
		}
		s.pathMax, err = s.readPathAt(f, int(s.numFiles)-1)
		if err != nil {
			return nil, fmt.Errorf("read pathMax: %w", err)
		}
	}

	// read trigram key table into memory (sorted uint32 array)
	if s.numTri > 0 {
		keyBytes := make([]byte, s.numTri*4)
		if _, err := f.ReadAt(keyBytes, int64(s.triIdx)); err != nil {
			return nil, fmt.Errorf("read trigram keys: %w", err)
		}
		s.triKeys = make([]uint32, s.numTri)
		for i := range s.triKeys {
			s.triKeys[i] = binary.LittleEndian.Uint32(keyBytes[i*4:])
		}
	}

	return s, nil
}

// readPathAt reads the path of file entry i from the open file.
func (s *Shard) readPathAt(f *os.File, i int) (string, error) {
	off := int64(s.fileTbl + i*fileEntryLen)
	var ent [6]byte
	if _, err := f.ReadAt(ent[:], off); err != nil {
		return "", err
	}
	pOff := binary.LittleEndian.Uint32(ent[0:])
	pLen := binary.LittleEndian.Uint16(ent[4:])
	buf := make([]byte, pLen)
	if _, err := f.ReadAt(buf, int64(s.pathBlob)+int64(pOff)); err != nil {
		return "", err
	}
	return string(buf), nil
}

// Path returns the filesystem path of the shard file.
func (s *Shard) Path() string { return s.path }

// PathMin returns the smallest path in this shard.
func (s *Shard) PathMin() string { return s.pathMin }

// PathMax returns the largest path in this shard.
func (s *Shard) PathMax() string { return s.pathMax }

// NumFiles returns the number of files in this shard.
func (s *Shard) NumFiles() int { return int(s.numFiles) }

// FileSize returns the on-disk size of the shard file.
func (s *Shard) FileSize() int { return s.size }

// Mmap maps the shard data into memory. Multiple concurrent callers
// share the same mapping (refcounted). Each Mmap call must be paired
// with a Munmap call.
func (s *Shard) Mmap() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refs > 0 {
		s.refs++
		return nil
	}
	f, err := os.Open(s.path)
	if err != nil {
		return err
	}
	data, err := mmapFile(f, s.size)
	if err != nil {
		f.Close()
		return err
	}
	s.data = data
	s.mapFile = f
	s.refs = 1
	return nil
}

// Munmap decrements the refcount and unmaps when it hits zero.
func (s *Shard) Munmap() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refs <= 0 {
		return
	}
	s.refs--
	if s.refs == 0 {
		munmapFile(s.data)
		s.mapFile.Close()
		s.data = nil
		s.mapFile = nil
	}
}

// ContainsPath reports whether path falls within [pathMin, pathMax].
func (s *Shard) ContainsPath(path string) bool {
	return path >= s.pathMin && path <= s.pathMax
}

// HasTrigrams reports whether all trigrams of the literal are present
// in this shard's key table. Uses the in-memory triKeys (no mmap needed).
// Returns true for short literals (< 3 bytes) since they match everything.
func (s *Shard) HasTrigrams(literal string) bool {
	if len(literal) < 3 {
		return true
	}
	tris := trigramsOfLit(literal)
	for _, tri := range tris {
		i := sort.Search(len(s.triKeys), func(j int) bool {
			return s.triKeys[j] >= tri
		})
		if i >= len(s.triKeys) || s.triKeys[i] != tri {
			return false
		}
	}
	return true
}

// TriKeyCount returns the number of trigram keys in this shard.
func (s *Shard) TriKeyCount() int { return len(s.triKeys) }

// File returns metadata for file ID i. Caller must hold a Mmap reference.
func (s *Shard) File(i int) FileMeta {
	off := s.fileTbl + i*fileEntryLen
	d := s.data[off:]
	pOff := binary.LittleEndian.Uint32(d[0:])
	pLen := binary.LittleEndian.Uint16(d[4:])
	return FileMeta{
		Path:    string(s.data[s.pathBlob+int(pOff) : s.pathBlob+int(pOff)+int(pLen)]),
		Size:    int64(binary.LittleEndian.Uint64(d[8:])),
		MtimeNS: int64(binary.LittleEndian.Uint64(d[16:])),
	}
}

// FileContent returns the inlined content for file ID i, or nil if the
// file was not inlined. Caller must hold a Mmap reference.
func (s *Shard) FileContent(i int) []byte {
	if s.contentOff == 0 {
		return nil
	}
	off := s.fileTbl + i*fileEntryLen
	d := s.data[off:]
	cOff := binary.LittleEndian.Uint32(d[24:])
	cLen := binary.LittleEndian.Uint32(d[28:])
	if cOff == noContent || cLen == 0 {
		return nil
	}
	base := s.contentOff + int(cOff)
	return s.data[base : base+int(cLen)]
}

// Files returns all file metadata. Caller must hold a Mmap reference.
func (s *Shard) Files() []FileMeta {
	out := make([]FileMeta, s.numFiles)
	for i := range out {
		out[i] = s.File(i)
	}
	return out
}

// FileDefs returns symbol definitions for file ID i from the symbol table.
// Caller must hold a Mmap reference. Returns nil if no symbols.
func (s *Shard) FileDefs(fileID int) []symbol.Def {
	if s.symbolOff == 0 || s.numSyms == 0 {
		return nil
	}
	// walk the symbol table from the start, skipping entries for files before fileID
	pos := s.symbolOff
	for fi := 0; fi <= fileID; fi++ {
		numDefs, next := readUvarint(s.data, pos)
		if fi == fileID {
			if numDefs == 0 {
				return nil
			}
			defs := make([]symbol.Def, numDefs)
			pos = next
			for di := range defs {
				nameLen, p := readUvarint(s.data, pos)
				name := string(s.data[p : p+int(nameLen)])
				p += int(nameLen)
				kindLen, p2 := readUvarint(s.data, p)
				kind := symbol.Kind(s.data[p2 : p2+int(kindLen)])
				p2 += int(kindLen)
				containerLen, p3 := readUvarint(s.data, p2)
				container := ""
				if containerLen > 0 {
					container = string(s.data[p3 : p3+int(containerLen)])
				}
				p3 += int(containerLen)
				line, p4 := readUvarint(s.data, p3)
				defs[di] = symbol.Def{Name: name, Kind: kind, Container: container, Line: int(line)}
				pos = p4
			}
			return defs
		}
		// skip this file's defs
		pos = next
		for di := uint64(0); di < numDefs; di++ {
			nameLen, p := readUvarint(s.data, pos)
			p += int(nameLen) // name
			kindLen, p2 := readUvarint(s.data, p)
			p2 += int(kindLen) // kind
			containerLen, p3 := readUvarint(s.data, p2)
			p3 += int(containerLen) // container
			_, p4 := readUvarint(s.data, p3) // line
			pos = p4
		}
	}
	return nil
}

// Candidates returns file IDs whose trigram sets contain all trigrams of
// the lowercased literal. Empty literal means all files.
// Caller must hold a Mmap reference.
func (s *Shard) Candidates(literal string) []uint32 {
	if len(literal) < 3 {
		all := make([]uint32, s.numFiles)
		for i := range all {
			all[i] = uint32(i)
		}
		return all
	}
	tris := trigramsOfLit(literal)
	var result []uint32
	for i, tri := range tris {
		ids := s.posting(tri)
		if ids == nil {
			return nil
		}
		if i == 0 {
			result = ids
		} else {
			result = intersect(result, ids)
			if len(result) == 0 {
				return nil
			}
		}
	}
	return result
}

func (s *Shard) posting(tri uint32) []uint32 {
	keysOff := s.triIdx
	n := int(s.numTri)
	lo, hi := 0, n
	for lo < hi {
		mid := lo + (hi-lo)/2
		k := binary.LittleEndian.Uint32(s.data[keysOff+mid*4:])
		if k < tri {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo >= n {
		return nil
	}
	if binary.LittleEndian.Uint32(s.data[keysOff+lo*4:]) != tri {
		return nil
	}
	offsBase := keysOff + n*4
	postOff := int(binary.LittleEndian.Uint32(s.data[offsBase+lo*4:])) + s.postings
	numDocs, pos := readUvarint(s.data, postOff)
	ids := make([]uint32, numDocs)
	var prev uint32
	for i := range ids {
		delta, next := readUvarint(s.data, pos)
		pos = next
		prev += uint32(delta)
		ids[i] = prev
	}
	return ids
}

func intersect(a, b []uint32) []uint32 {
	out := a[:0:0]
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] < b[j]:
			i++
		case a[i] > b[j]:
			j++
		default:
			out = append(out, a[i])
			i++
			j++
		}
	}
	return out
}

func lower(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + 32
	}
	return b
}

func trigramsOfLit(s string) []uint32 {
	seen := make(map[uint32]struct{})
	b := []byte(s)
	for i := 0; i+3 <= len(b); i++ {
		k := uint32(lower(b[i]))<<16 | uint32(lower(b[i+1]))<<8 | uint32(lower(b[i+2]))
		seen[k] = struct{}{}
	}
	out := make([]uint32, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Load reads a shard file entirely into memory (Go heap). Used only by
// tests and the builder's verify step. Production code uses Open+Mmap.
func Load(path string) (*Shard, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < headerSize {
		return nil, fmt.Errorf("shard too small: %d bytes", len(data))
	}
	if [8]byte(data[:8]) != magic {
		return nil, fmt.Errorf("bad magic")
	}
	ver := binary.LittleEndian.Uint32(data[8:])
	if ver != formatVer {
		return nil, fmt.Errorf("unsupported version %d", ver)
	}
	s := &Shard{path: path, size: len(data)}
	s.numFiles = binary.LittleEndian.Uint32(data[12:])
	s.numTri = binary.LittleEndian.Uint32(data[16:])
	s.numSyms = binary.LittleEndian.Uint32(data[20:])
	s.fileTbl = int(binary.LittleEndian.Uint64(data[24:]))
	s.triIdx = int(binary.LittleEndian.Uint64(data[32:]))
	s.postings = int(binary.LittleEndian.Uint64(data[40:]))
	s.symbolOff = int(binary.LittleEndian.Uint64(data[48:]))
	s.contentOff = int(binary.LittleEndian.Uint64(data[56:]))
	s.pathBlob = s.fileTbl + int(s.numFiles)*fileEntryLen
	s.data = data
	s.refs = 1 // permanently mapped
	if s.numFiles > 0 {
		s.pathMin = s.File(0).Path
		s.pathMax = s.File(int(s.numFiles) - 1).Path
	}
	if s.numTri > 0 {
		s.triKeys = make([]uint32, s.numTri)
		for i := range s.triKeys {
			s.triKeys[i] = binary.LittleEndian.Uint32(data[s.triIdx+i*4:])
		}
	}
	return s, nil
}
