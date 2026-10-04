package services

import (
	"path/filepath"
	"testing"
	"time"
)

// Two goroutines of one storage share its lock descriptors, and flock does not
// tell them apart: the second one's "upgrade" succeeds on the spot, and its
// downgrade hands the directory to other holders while the first is still
// punching.
func TestWhileAloneHoldsTheDirAcrossGoroutines(t *testing.T) {
	dataDir := zSeed(t)
	impl := zOpen(t, dataDir, nil)
	defer impl.Close()
	ts := zStorage(impl)

	punching, punched := make(chan struct{}), make(chan struct{})
	first := make(chan bool)
	go func() { first <- ts.whileAlone(func() { close(punching); <-punched }) }()
	<-punching
	second := make(chan bool)
	go func() { second <- ts.whileAlone(func() {}) }()

	other := make(chan struct{})
	go func() {
		f, err := lockDir(filepath.Join(dataDir, zHash))
		if err == nil {
			_ = f.Close()
		}
		close(other)
	}()
	select {
	case <-other:
		t.Error("another holder took the dir while an eviction had it exclusive")
	case <-time.After(300 * time.Millisecond):
	}
	close(punched)
	<-other
	if !<-first {
		t.Error("first eviction was not alone")
	}
	<-second
}

// On Linux a failed upgrade leaves the storage holding nothing until it takes
// the lock shared again. Another storage of the torrent must not get the dir
// exclusive in that gap: it would punch holes the first one still counts as
// data. Only a Linux run can fail this; macOS keeps the shared lock.
func TestEvictGateShutsTheFailedUpgradeGap(t *testing.T) {
	dataDir := zSeed(t)
	x := zOpen(t, dataDir, nil)
	defer x.Close()
	y := zOpen(t, dataDir, nil)
	defer y.Close()

	yAlone := make(chan bool, 1)
	zStorage(x).afterFailedUpgrade = func() {
		go func() { yAlone <- zStorage(y).whileAlone(func() {}) }()
		select {
		case alone := <-yAlone:
			yAlone <- alone
		case <-time.After(300 * time.Millisecond): // y waits for the gate
		}
	}
	if zStorage(x).whileAlone(func() {}) {
		t.Fatal("x had the dir exclusive while y held it")
	}
	if <-yAlone {
		t.Error("y got the dir exclusive in x's failed-upgrade gap, while x still had the torrent")
	}
}
