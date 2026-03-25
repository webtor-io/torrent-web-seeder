//go:build linux

package services

import (
	"os"

	"golang.org/x/sys/unix"
)

// punchHole uses fallocate to deallocate blocks in a file without changing its size.
// The mmap remains valid — reads of punched regions return zeros, writes allocate new blocks.
func punchHole(f *os.File, offset, length int64) error {
	return unix.Fallocate(int(f.Fd()), unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, offset, length)
}
