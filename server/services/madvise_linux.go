//go:build linux

package services

import (
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

var pageSize = int64(os.Getpagesize())

// madviseEvict advises the kernel that the given mmap'd region is no longer needed,
// allowing immediate page reclamation. This reduces RSS/cgroup memory pressure
// after hole-punching evicted pieces.
//
// The byte slice must come from an mmap'd region. The function handles
// page-alignment internally — madvise(2) requires the start address to be
// page-aligned, but piece boundaries rarely are.
func madviseEvict(b []byte) error {
	if len(b) == 0 {
		return nil
	}

	// Compute the address of b[0] and align it down to a page boundary.
	addr := uintptr(unsafe.Pointer(&b[0]))
	aligned := addr &^ uintptr(pageSize-1)
	// Extend the length to cover the rounded-down start and the original end.
	length := uintptr(len(b)) + (addr - aligned)

	// Build a slice starting at the page-aligned address.
	alignedSlice := unsafe.Slice((*byte)(unsafe.Pointer(aligned)), length)

	return unix.Madvise(alignedSlice, unix.MADV_DONTNEED)
}
