package services

import (
	"context"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent/storage"
	"github.com/prometheus/client_golang/prometheus/testutil"
	log "github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
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

// A cache-path stream holds the torrent's dir, and an eviction of the torrent
// that a pod tries during the stream waits for it (the cache path leaves a
// torrent that is evicted already to the torrent, so it is one that a pod
// started evicting after the stream began). A client that stops reading must
// not hold it for as long as it stays connected: after stallTimeout without
// progress the stream lets go.
func TestCachePathStalledStreamLetsGoOfTheDir(t *testing.T) {
	dataDir := zSeed(t) // rows written: b is served from cache
	info := zInfo()

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String(DataDirFlag, dataDir, "")
	s := &WebSeeder{fcm: zFCM(dataDir), tom: NewTouchMap(cli.NewContext(nil, fs, nil)), stallTimeout: time.Second}
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

	impl := zOpen(t, dataDir, nil)
	defer impl.Close()
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
			t.Fatal("a stalled cache-path stream still holds the dir 5 s after its 1 s stall timeout")
		}
	}
}

// parkUncached lets resource.cached through and parks resource.uncached until
// released, as a NATS publish on a stalled socket would: nats.go writes to the
// socket synchronously once its buffer is full, for up to its one-minute
// flusher timeout.
type parkUncached struct {
	cached  chan string
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *parkUncached) Publish(subject string, data []byte) error {
	if subject == cachedSubject {
		p.cached <- subject + " " + string(data)
		return nil
	}
	p.once.Do(func() { close(p.entered) })
	<-p.release
	return nil
}

// An eviction holds the torrent's dir exclusive, and every other pod's
// OpenTorrent of the torrent waits for it in lockDir -- under the library's
// client lock (setInfo), so that pod's whole client waits with it. Nothing in
// there may wait on the network. The Uncached publish of the evicted piece's
// file did.
func TestEvictionPublishesOutsideTheDir(t *testing.T) {
	dataDir := zSeed(t)
	info := zInfo()
	pub := &parkUncached{cached: make(chan string, 16), entered: make(chan struct{}), release: make(chan struct{})}
	impl := zOpen(t, dataDir, pub)
	defer impl.Close()
	defer close(pub.release)         // before Close, which waits for the eviction
	for got := ""; got != cachedD; { // b announced: its eviction publishes
		select {
		case got = <-pub.cached:
		case <-time.After(10 * time.Second):
			t.Fatal("completion loop never announced d")
		}
	}

	zTouch(t, impl, info, 0)
	zTouch(t, impl, info, 3)
	go func() { // c's piece arrives; the LRU evicts piece 1, b's tail
		p := info.Piece(2)
		sp := impl.Piece(p)
		_, _ = sp.WriteAt(zData()[p.Offset():p.Offset()+p.Length()], 0)
		_ = sp.MarkComplete()
	}()
	select {
	case <-pub.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("eviction of piece 1 never published Uncached(b)")
	}

	opened := make(chan error, 1)
	go func() {
		pod2, err := NewMMap(dataDir, 0, FileCacheConfig{}).OpenTorrent(context.Background(), info, zIH)
		if err == nil {
			err = pod2.Close()
		}
		opened <- err
	}()
	select {
	case err := <-opened:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Error("another pod's OpenTorrent waited 2 s for the dir while the eviction's Uncached publish was stuck")
	}
}

// An eviction waiting for the dir (aloneMu, the gate) when the library drops
// the torrent ran in the middle of Close, which told whileAlone to stop only
// in its last step. A punch after Close had taken the LRU off the gauges took
// its piece off a second time; one after the completion db closed failed with
// "failed to mark piece N incomplete during eviction: closed". 2026-10-05,
// worker63: 11 such lines and cache_pieces_count at -1 and -3 on the two pods,
// each time within 6 s of dropping a torrent that was evicting.
func TestCloseRefusesEvictions(t *testing.T) {
	hook := test.NewGlobal()
	defer hook.Reset()
	before := testutil.ToFloat64(promCachePieceCount)
	impl := zOpen(t, zSeed(t), nil)
	ts := zStorage(impl)

	shard := ts.pieceLock(1)
	shard.Lock() // the eviction of piece 1 stops mid-way, holding aloneMu
	evicted := make(chan struct{})
	go func() { ts.evictPiece(1); close(evicted) }()
	for ts.aloneMu.TryLock() {
		ts.aloneMu.Unlock()
		time.Sleep(time.Millisecond)
	}
	dbClosing := make(chan struct{}, 1)
	ts.pc = closeHookPC{ts.pc, func(closeDB func() error) error {
		dbClosing <- struct{}{}
		<-evicted
		err := closeDB()
		ts.evictPiece(3)
		return err
	}}
	closed := make(chan error)
	go func() { closed <- impl.Close() }()
	select {
	case <-dbClosing: // Close went on past the gauges
	case <-time.After(200 * time.Millisecond): // Close waits for the eviction
	}
	shard.Unlock()
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if d := testutil.ToFloat64(promCachePieceCount) - before; d != 0 {
		t.Errorf("cache_pieces_count is off by %v once the storage closed", d)
	}
	for _, e := range hook.AllEntries() {
		if strings.Contains(e.Message, "during eviction") {
			t.Errorf("an eviction ran in Close: %s: %v", e.Message, e.Data[log.ErrorKey])
		}
	}
}

// closeHookPC runs close in place of Close, handing it the real one.
type closeHookPC struct {
	storage.PieceCompletion
	close func(real func() error) error
}

func (p closeHookPC) Close() error { return p.close(p.PieceCompletion.Close) }
