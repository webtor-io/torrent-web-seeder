package services

import (
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/urfave/cli"
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

// stuckWriter is a client that stopped reading: Write blocks until unblocked.
type stuckWriter struct {
	h       http.Header
	once    sync.Once
	writing chan struct{}
	unblock chan struct{}
}

func (w *stuckWriter) Header() http.Header { return w.h }
func (w *stuckWriter) WriteHeader(int)     {}
func (w *stuckWriter) Write([]byte) (int, error) {
	w.once.Do(func() { close(w.writing) })
	<-w.unblock
	return 0, errors.New("client gone")
}

// A cache-path stream holds the torrent's dir, and every eviction of the
// torrent on the node waits for it. A client that stops reading must not hold
// it for as long as it stays connected: after stallTimeout without progress
// the stream lets go.
func TestCachePathStalledStreamLetsGoOfTheDir(t *testing.T) {
	dataDir := zSeed(t)
	info := zInfo()
	gate := newGatePub()
	close(gate.release)
	impl := zOpen(t, dataDir, gate)
	defer impl.Close()
	gate.wait(t, cachedD) // rows written: b is served from cache

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String(DataDirFlag, dataDir, "")
	s := &WebSeeder{fcm: zFCM(dataDir), tom: NewTouchMap(cli.NewContext(nil, fs, nil)), stallTimeout: 200 * time.Millisecond}
	w := &stuckWriter{h: http.Header{}, writing: make(chan struct{}), unblock: make(chan struct{})}
	served := make(chan struct{})
	go func() {
		s.serveFile(w, httptest.NewRequest(http.MethodGet, "/"+zHash+"/pack/b.mkv", nil), zHash, "pack/b.mkv")
		close(served)
	}()
	defer func() { close(w.unblock); <-served }()
	select {
	case <-w.writing:
	case <-time.After(10 * time.Second):
		t.Fatal("b was not served from cache")
	}

	zTouch(t, impl, info, 0)
	zTouch(t, impl, info, 3)
	zWritePiece(t, impl, info, 2) // over budget; put off by the stream
	ts := zStorage(impl)
	if ts.isEvicted(1) {
		t.Fatal("piece 1 evicted under the stream")
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		ts.evictOverBudget()
		if ts.isEvicted(1) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("a stalled cache-path stream still holds the dir 5 s after its 200 ms stall timeout")
		}
	}
}
