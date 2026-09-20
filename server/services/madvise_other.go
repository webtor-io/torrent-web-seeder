//go:build !linux

package services

import "os"

// madviseEvict is a no-op on non-Linux platforms.
func madviseEvict(_ []byte) error { return nil }

// madviseSequential is a no-op on non-Linux platforms.
func madviseSequential(_ []byte) error { return nil }

// fadviseEvict is a no-op on non-Linux platforms.
func fadviseEvict(_ *os.File, _, _ int64) error { return nil }
