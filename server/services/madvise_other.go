//go:build !linux

package services

// madviseEvict is a no-op on non-Linux platforms.
func madviseEvict(_ []byte) error {
	return nil
}
