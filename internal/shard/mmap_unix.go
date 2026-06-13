//go:build !windows

package shard

import (
	"os"
	"syscall"
)

func mmapFile(f *os.File, size int) ([]byte, error) {
	return syscall.Mmap(int(f.Fd()), 0, size, syscall.PROT_READ, syscall.MAP_SHARED)
}

func munmapFile(b []byte) error {
	return syscall.Munmap(b)
}
