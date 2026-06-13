package shard

import (
	"encoding/binary"
	"os"
	"sort"
)

// Builder accumulates files and their trigrams, then writes a shard file.
type Builder struct {
	files []buildFile
	tris  map[uint32][]uint32 // trigram → sorted file IDs
}

type buildFile struct {
	path    string
	size    int64
	mtimeNS int64
}

func NewBuilder() *Builder {
	return &Builder{tris: make(map[uint32][]uint32)}
}

// Add registers a file and its trigram set. content is only used for
// trigram extraction and is not stored. fileID is the 0-based index
// in the order of Add calls.
func (b *Builder) Add(path string, size, mtimeNS int64, content []byte) {
	id := uint32(len(b.files))
	b.files = append(b.files, buildFile{path: path, size: size, mtimeNS: mtimeNS})
	seen := make(map[uint32]struct{})
	for i := 0; i+3 <= len(content); i++ {
		k := uint32(lower(content[i]))<<16 | uint32(lower(content[i+1]))<<8 | uint32(lower(content[i+2]))
		seen[k] = struct{}{}
	}
	for k := range seen {
		b.tris[k] = append(b.tris[k], id)
	}
}

// NumFiles returns how many files have been added.
func (b *Builder) NumFiles() int { return len(b.files) }

// Write serializes the shard to path. The file is written atomically
// (tmp + rename).
func (b *Builder) Write(path string) error {
	// sort trigram keys
	triKeys := make([]uint32, 0, len(b.tris))
	for k := range b.tris {
		triKeys = append(triKeys, k)
	}
	sort.Slice(triKeys, func(i, j int) bool { return triKeys[i] < triKeys[j] })

	// build path blob
	var pathBlob []byte
	type pathRef struct{ off, len int }
	pathRefs := make([]pathRef, len(b.files))
	for i, f := range b.files {
		pathRefs[i] = pathRef{off: len(pathBlob), len: len(f.path)}
		pathBlob = append(pathBlob, f.path...)
	}

	// build postings
	var postData []byte
	postOffsets := make([]uint32, len(triKeys))
	for i, k := range triKeys {
		ids := b.tris[k]
		sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })
		postOffsets[i] = uint32(len(postData))
		postData = appendUvarint(postData, uint64(len(ids)))
		var prev uint32
		for _, id := range ids {
			postData = appendUvarint(postData, uint64(id-prev))
			prev = id
		}
	}

	numFiles := uint32(len(b.files))
	numTri := uint32(len(triKeys))

	fileTblOff := uint64(headerSize)
	pathBlobSize := len(pathBlob)
	triIdxOff := fileTblOff + uint64(numFiles)*fileEntryLen + uint64(pathBlobSize)
	triIdxSize := uint64(numTri) * 4 * 2 // keys + offsets
	postOff := triIdxOff + triIdxSize

	totalSize := int(postOff) + len(postData)
	out := make([]byte, totalSize)

	// header
	copy(out[0:], magic[:])
	binary.LittleEndian.PutUint32(out[8:], formatVer)
	binary.LittleEndian.PutUint32(out[12:], numFiles)
	binary.LittleEndian.PutUint32(out[16:], numTri)
	binary.LittleEndian.PutUint64(out[24:], fileTblOff)
	binary.LittleEndian.PutUint64(out[32:], triIdxOff)
	binary.LittleEndian.PutUint64(out[40:], postOff)

	// file table
	for i, f := range b.files {
		off := int(fileTblOff) + i*fileEntryLen
		binary.LittleEndian.PutUint32(out[off:], uint32(pathRefs[i].off))
		binary.LittleEndian.PutUint16(out[off+4:], uint16(pathRefs[i].len))
		binary.LittleEndian.PutUint64(out[off+8:], uint64(f.size))
		binary.LittleEndian.PutUint64(out[off+16:], uint64(f.mtimeNS))
	}

	// path blob
	copy(out[int(fileTblOff)+int(numFiles)*fileEntryLen:], pathBlob)

	// trigram index
	for i, k := range triKeys {
		binary.LittleEndian.PutUint32(out[int(triIdxOff)+i*4:], k)
	}
	offsBase := int(triIdxOff) + int(numTri)*4
	for i, o := range postOffsets {
		binary.LittleEndian.PutUint32(out[offsBase+i*4:], o)
	}

	// postings
	copy(out[int(postOff):], postData)

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
