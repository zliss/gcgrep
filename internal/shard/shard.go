// Package shard implements an immutable, mmap-friendly trigram index
// stored as a single binary file. A shard covers a group of source files
// (typically ~10k files or ~256MB of source); several shards plus a dirty
// list make up the disk engine for one indexed root.
//
// File format (little-endian):
//
//	Header (48 bytes):
//	  magic      [8]byte  "GCSHARD1"
//	  version    uint32   1
//	  numFiles   uint32
//	  numTri     uint32
//	  _          uint32
//	  fileTblOff uint64
//	  triIdxOff  uint64
//	  postOff    uint64
//
//	File table (at fileTblOff, 24 bytes per entry):
//	  pathOff  uint32   (into path blob)
//	  pathLen  uint16
//	  _        uint16
//	  size     int64
//	  mtimeNS  int64
//
//	Path blob (after file table):
//	  concatenated UTF-8 relative paths
//
//	Trigram index (at triIdxOff):
//	  keys:    numTri × uint32 (sorted)
//	  offsets: numTri × uint32 (posting byte offset relative to postOff)
//
//	Postings (at postOff):
//	  per trigram: uvarint(numDocs), then numDocs uvarint-delta file IDs
package shard

import (
	"encoding/binary"
	"fmt"
	"os"
	"sort"
)

var magic = [8]byte{'G', 'C', 'S', 'H', 'A', 'R', 'D', '1'}

const (
	headerSize   = 48
	fileEntryLen = 24
	formatVer    = 1
)

// FileMeta is the per-file metadata stored in a shard.
type FileMeta struct {
	Path    string
	Size    int64
	MtimeNS int64
}

// Shard is a loaded (read-only) shard file.
type Shard struct {
	data []byte // entire file contents (OS will page via file cache)

	numFiles uint32
	numTri   uint32
	fileTbl  int // byte offset of file table
	pathBlob int // byte offset of path blob
	triIdx   int // byte offset of trigram index
	postings int // byte offset of postings
}

// Load reads a shard file into memory.
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
	s := &Shard{data: data}
	s.numFiles = binary.LittleEndian.Uint32(data[12:])
	s.numTri = binary.LittleEndian.Uint32(data[16:])
	s.fileTbl = int(binary.LittleEndian.Uint64(data[24:]))
	s.triIdx = int(binary.LittleEndian.Uint64(data[32:]))
	s.postings = int(binary.LittleEndian.Uint64(data[40:]))
	s.pathBlob = s.fileTbl + int(s.numFiles)*fileEntryLen
	return s, nil
}

// NumFiles returns the number of files in this shard.
func (s *Shard) NumFiles() int { return int(s.numFiles) }

// File returns metadata for file ID i.
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

// Files returns all file metadata.
func (s *Shard) Files() []FileMeta {
	out := make([]FileMeta, s.numFiles)
	for i := range out {
		out[i] = s.File(i)
	}
	return out
}

// Candidates returns file IDs whose trigram sets contain all trigrams of
// the lowercased literal. Empty literal means all files.
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
	// binary search in the sorted key array
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
	// read posting offset
	offsBase := keysOff + n*4
	postOff := int(binary.LittleEndian.Uint32(s.data[offsBase+lo*4:])) + s.postings
	// decode varint posting list
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
