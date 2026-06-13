package shard

import "encoding/binary"

func putUvarint(buf []byte, x uint64) int { return binary.PutUvarint(buf, x) }

func appendUvarint(b []byte, x uint64) []byte {
	var buf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(buf[:], x)
	return append(b, buf[:n]...)
}

func readUvarint(data []byte, pos int) (uint64, int) {
	v, n := binary.Uvarint(data[pos:])
	return v, pos + n
}
