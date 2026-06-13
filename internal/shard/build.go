package shard

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zliss/gcgrep/internal/symbol"
)

// ShardSizeConfig holds the shard sizing parameters.
type ShardSizeConfig struct {
	TargetBytes int64
	MinBytes    int64
	MaxBytes    int64
}

type buildFile struct {
	path    string
	size    int64
	mtimeNS int64
	defs    []symbol.Def
	content []byte // non-nil only when inlined (≤ inlineKB threshold)
}

func NewBuilder(inlineKB int) *Builder {
	return &Builder{tris: make(map[uint32][]uint32), inlineKB: inlineKB}
}

// Builder accumulates files and their trigrams, then writes a shard file.
type Builder struct {
	files    []buildFile
	tris     map[uint32][]uint32
	inlineKB int // files ≤ this size (KB) have content stored in shard; 0 = none
}

// Add registers a file, its trigram set, and symbol definitions.
// content is used for trigram extraction and optionally inlined in the shard.
func (b *Builder) Add(path string, size, mtimeNS int64, content []byte) {
	b.AddWithDefs(path, size, mtimeNS, content, nil)
}

// AddWithDefs is like Add but also stores pre-extracted symbol definitions.
func (b *Builder) AddWithDefs(path string, size, mtimeNS int64, content []byte, defs []symbol.Def) {
	id := uint32(len(b.files))
	var stored []byte
	if b.inlineKB > 0 && len(content) <= b.inlineKB*1024 {
		stored = make([]byte, len(content))
		copy(stored, content)
	}
	b.files = append(b.files, buildFile{path: path, size: size, mtimeNS: mtimeNS, defs: defs, content: stored})
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

// Write serializes the shard to path atomically (tmp+rename).
func (b *Builder) Write(path string) error {
	triKeys := make([]uint32, 0, len(b.tris))
	for k := range b.tris {
		triKeys = append(triKeys, k)
	}
	sort.Slice(triKeys, func(i, j int) bool { return triKeys[i] < triKeys[j] })

	var pathBlob []byte
	type pathRef struct{ off, len int }
	pathRefs := make([]pathRef, len(b.files))
	for i, f := range b.files {
		pathRefs[i] = pathRef{off: len(pathBlob), len: len(f.path)}
		pathBlob = append(pathBlob, f.path...)
	}

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

	// build symbol section
	var symData []byte
	var numSyms uint32
	for _, f := range b.files {
		symData = appendUvarint(symData, uint64(len(f.defs)))
		for _, d := range f.defs {
			symData = appendUvarint(symData, uint64(len(d.Name)))
			symData = append(symData, d.Name...)
			symData = appendUvarint(symData, uint64(len(d.Kind)))
			symData = append(symData, d.Kind...)
			symData = appendUvarint(symData, uint64(len(d.Container)))
			symData = append(symData, d.Container...)
			symData = appendUvarint(symData, uint64(d.Line))
			numSyms++
		}
	}

	// build content blob
	var contentBlob []byte
	type contentRef struct {
		off uint32
		len uint32
	}
	contentRefs := make([]contentRef, len(b.files))
	for i, f := range b.files {
		if f.content != nil {
			contentRefs[i] = contentRef{off: uint32(len(contentBlob)), len: uint32(len(f.content))}
			contentBlob = append(contentBlob, f.content...)
		} else {
			contentRefs[i] = contentRef{off: noContent, len: 0}
		}
	}

	numFiles := uint32(len(b.files))
	numTri := uint32(len(triKeys))

	fileTblOff := uint64(headerSize)
	pathBlobSize := len(pathBlob)
	triIdxOff := fileTblOff + uint64(numFiles)*fileEntryLen + uint64(pathBlobSize)
	triIdxSize := uint64(numTri) * 4 * 2
	postOff := triIdxOff + triIdxSize
	symOff := postOff + uint64(len(postData))
	var ctOff uint64
	if len(contentBlob) > 0 {
		ctOff = symOff + uint64(len(symData))
	}

	totalSize := int(symOff) + len(symData) + len(contentBlob)
	out := make([]byte, totalSize)

	copy(out[0:], magic[:])
	binary.LittleEndian.PutUint32(out[8:], formatVer)
	binary.LittleEndian.PutUint32(out[12:], numFiles)
	binary.LittleEndian.PutUint32(out[16:], numTri)
	binary.LittleEndian.PutUint32(out[20:], numSyms)
	binary.LittleEndian.PutUint64(out[24:], fileTblOff)
	binary.LittleEndian.PutUint64(out[32:], triIdxOff)
	binary.LittleEndian.PutUint64(out[40:], postOff)
	binary.LittleEndian.PutUint64(out[48:], symOff)
	binary.LittleEndian.PutUint64(out[56:], ctOff)

	for i, f := range b.files {
		off := int(fileTblOff) + i*fileEntryLen
		binary.LittleEndian.PutUint32(out[off:], uint32(pathRefs[i].off))
		binary.LittleEndian.PutUint16(out[off+4:], uint16(pathRefs[i].len))
		binary.LittleEndian.PutUint64(out[off+8:], uint64(f.size))
		binary.LittleEndian.PutUint64(out[off+16:], uint64(f.mtimeNS))
		binary.LittleEndian.PutUint32(out[off+24:], contentRefs[i].off)
		binary.LittleEndian.PutUint32(out[off+28:], contentRefs[i].len)
	}

	copy(out[int(fileTblOff)+int(numFiles)*fileEntryLen:], pathBlob)

	for i, k := range triKeys {
		binary.LittleEndian.PutUint32(out[int(triIdxOff)+i*4:], k)
	}
	offsBase := int(triIdxOff) + int(numTri)*4
	for i, o := range postOffsets {
		binary.LittleEndian.PutUint32(out[offsBase+i*4:], o)
	}

	copy(out[int(postOff):], postData)
	copy(out[int(symOff):], symData)
	if ctOff > 0 {
		copy(out[int(ctOff):], contentBlob)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// BuildFile is a file discovered by the walker, passed to FullBuild/Reconcile.
type BuildFile struct {
	Rel     string
	Size    int64
	MtimeNS int64
}

// GroupByDirBoundary partitions sorted files into shard-sized batches,
// splitting on directory boundaries at ~targetBytes.
func GroupByDirBoundary(files []BuildFile, sc ShardSizeConfig) [][]BuildFile {
	sort.Slice(files, func(i, j int) bool { return files[i].Rel < files[j].Rel })
	if len(files) == 0 {
		return nil
	}

	var batches [][]BuildFile
	var cur []BuildFile
	var curBytes int64

	for _, f := range files {
		cur = append(cur, f)
		curBytes += f.Size

		if curBytes >= sc.TargetBytes {
			// try to split at directory boundary
			batches = append(batches, cur)
			cur = nil
			curBytes = 0
		}
	}
	if len(cur) > 0 {
		// merge tiny last batch into previous if below minimum
		if len(batches) > 0 && curBytes < sc.MinBytes {
			batches[len(batches)-1] = append(batches[len(batches)-1], cur...)
		} else {
			batches = append(batches, cur)
		}
	}
	return batches
}

// topDir returns the top-level directory of a slash-separated path.
func topDir(rel string) string {
	if i := strings.IndexByte(rel, '/'); i >= 0 {
		return rel[:i]
	}
	return ""
}
