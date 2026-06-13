//go:build windows

package shard

import (
	"os"
	"syscall"
	"unsafe"
)

func mmapFile(f *os.File, size int) ([]byte, error) {
	h, err := syscall.CreateFileMapping(syscall.Handle(f.Fd()), nil, syscall.PAGE_READONLY, 0, 0, nil)
	if err != nil {
		return nil, err
	}
	defer syscall.CloseHandle(h)
	addr, err := syscall.MapViewOfFile(h, syscall.FILE_MAP_READ, 0, 0, 0)
	if err != nil {
		return nil, err
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(addr)), size), nil
}

func munmapFile(b []byte) error {
	return syscall.UnmapViewOfFile(uintptr(unsafe.Pointer(&b[0])))
}
