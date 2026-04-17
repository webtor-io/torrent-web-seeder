//go:build linux

package services

import "golang.org/x/sys/unix"

// madviseEvict advises the kernel that the given mmap'd region is no longer needed,
// allowing immediate page reclamation. This reduces RSS/cgroup memory pressure
// after hole-punching evicted pieces.
func madviseEvict(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	return unix.Madvise(b, unix.MADV_DONTNEED)
}
