//go:build linux

package services

import (
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

var pageSize = int64(os.Getpagesize())

// pageAlignedSlice returns a byte slice whose start address is rounded down
// to a page boundary and whose length covers the original range.
// madvise(2) requires page-aligned addresses.
func pageAlignedSlice(b []byte) []byte {
	addr := uintptr(unsafe.Pointer(&b[0]))
	aligned := addr &^ uintptr(pageSize-1)
	length := uintptr(len(b)) + (addr - aligned)
	return unsafe.Slice((*byte)(unsafe.Pointer(aligned)), length)
}

// madviseEvict advises the kernel that the given mmap'd region is no longer needed
// (MADV_DONTNEED), allowing immediate page reclamation from RSS/cgroup memory.
func madviseEvict(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	return unix.Madvise(pageAlignedSlice(b), unix.MADV_DONTNEED)
}

// madviseSequential hints that the mmap'd region will be accessed sequentially.
// The kernel will use aggressive readahead and free pages after they are read.
func madviseSequential(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	return unix.Madvise(pageAlignedSlice(b), unix.MADV_SEQUENTIAL)
}

// fadviseEvict tells the kernel the given byte range of an open file is no
// longer needed (POSIX_FADV_DONTNEED): the pread counterpart of madviseEvict,
// so file pages read through a descriptor do not pile up as page cache
// charged to the pod. Dirty pages are skipped by the kernel; they leave
// after writeback.
func fadviseEvict(f *os.File, off, length int64) error {
	if f == nil || length <= 0 {
		return nil
	}
	return unix.Fadvise(int(f.Fd()), off, length, unix.FADV_DONTNEED)
}
