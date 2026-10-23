//go:build linux

package services

import (
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

func TestPageAlignedSliceUnalignedRange(t *testing.T) {
	ps := int(pageSize)
	m, err := unix.Mmap(-1, 0, 3*ps, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_ANON|unix.MAP_PRIVATE)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Munmap(m)

	b := m[ps+100 : 2*ps+5]
	a := pageAlignedSlice(b)
	if unsafe.Pointer(&a[0]) != unsafe.Pointer(&m[ps]) {
		t.Fatalf("start not rounded down to the page: off %d", uintptr(unsafe.Pointer(&a[0]))-uintptr(unsafe.Pointer(&m[0])))
	}
	if unsafe.Pointer(&a[len(a)-1]) != unsafe.Pointer(&b[len(b)-1]) {
		t.Fatalf("end moved: len %d, want %d", len(a), ps+5)
	}
	if err := madviseEvict(b); err != nil {
		t.Fatal(err)
	}
}
