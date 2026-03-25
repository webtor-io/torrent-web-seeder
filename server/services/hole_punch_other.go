//go:build !linux

package services

import "os"

// punchHole is a no-op stub for non-Linux platforms.
// On macOS/Windows there is no fallocate(PUNCH_HOLE), so we just zero out the region.
// This does NOT free disk blocks but keeps the logic correct for development.
func punchHole(f *os.File, offset, length int64) error {
	buf := make([]byte, 32*1024) // 32KB chunks to avoid large allocations
	for length > 0 {
		n := int64(len(buf))
		if n > length {
			n = length
		}
		if _, err := f.WriteAt(buf[:n], offset); err != nil {
			return err
		}
		offset += n
		length -= n
	}
	return nil
}
