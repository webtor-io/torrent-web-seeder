package services

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

type fakeReader struct {
	closed  atomic.Int32
	pos     int64
	size    int64
	onClose func()
}

func (f *fakeReader) Read(p []byte) (int, error) {
	if f.pos >= f.size {
		return 0, io.EOF
	}
	n := int64(len(p))
	if f.pos+n > f.size {
		n = f.size - f.pos
	}
	f.pos += n
	return int(n), nil
}
func (f *fakeReader) Seek(o int64, w int) (int64, error) {
	switch w {
	case io.SeekStart:
		f.pos = o
	case io.SeekCurrent:
		f.pos += o
	case io.SeekEnd:
		f.pos = f.size + o
	}
	return f.pos, nil
}
func (f *fakeReader) Close() error {
	if f.onClose != nil {
		f.onClose()
	}
	f.closed.Add(1)
	return nil
}

// fakePieces is a torrent's explicit priorities as the linger leaves them.
type fakePieces struct {
	done   map[int]bool // complete pieces
	wanted map[int]bool
}

func newFakePieces(complete ...int) *fakePieces {
	f := &fakePieces{done: map[int]bool{}, wanted: map[int]bool{}}
	for _, i := range complete {
		f.done[i] = true
	}
	return f
}

func (f *fakePieces) complete(i int) bool     { return f.done[i] }
func (f *fakePieces) want(i int)              { f.wanted[i] = true }
func (f *fakePieces) unwant(i int)            { delete(f.wanted, i) }
func (f *fakePieces) closed() <-chan struct{} { return nil }

// String lists the wanted pieces in order, "3 4 5 6".
func (f *fakePieces) String() string {
	var s []string
	for i := 0; i < 16; i++ {
		if f.wanted[i] {
			s = append(s, fmt.Sprint(i))
		}
	}
	return strings.Join(s, " ")
}

// timers stands in for time.AfterFunc; fire runs one, fireAll the rest.
type timers struct{ pending []func() }

func (ts *timers) after(_ time.Duration, fn func()) *time.Timer {
	ts.pending = append(ts.pending, fn)
	return nil
}
func (ts *timers) fire(i int) {
	ts.pending[i]()
	ts.pending[i] = nil
}
func (ts *timers) fireAll() {
	for i, fn := range ts.pending {
		if fn != nil {
			ts.fire(i)
		}
	}
}

func testLinger(d time.Duration) (*Linger, *timers) {
	l := newLinger(d, 2500)
	ts := &timers{}
	l.afterFn = ts.after
	return l, ts
}

// File starts 1000 bytes into the torrent, 10 000 bytes long, 1 KiB
// pieces (so it spans pieces 0..10), readahead 2500 bytes.
var testWindow = window{fileOffset: 1000, fileLength: 10000, pieceLen: 1024}

// Close holds the pieces of [pos, pos+readahead) before it closes the
// reader, and releases exactly that range when the linger elapses.
func TestLinger_HoldsWindowOnCloseAndReleasesLater(t *testing.T) {
	l, ts := testLinger(90 * time.Second)
	fr := &fakeReader{size: 10000}
	fp := newFakePieces()
	wantedAtClose := ""
	fr.onClose = func() { wantedAtClose = fp.String() }
	gauge := testutil.ToFloat64(promLingerWindows)
	r := l.wrap(fr, fp, testWindow)
	// The client read 3000 bytes then dropped: pos = 3000 → torrent offset
	// 4000, window [4000, 6500) → pieces 3..6.
	if _, err := io.CopyN(io.Discard, r, 3000); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if fr.closed.Load() != 1 {
		t.Fatal("underlying reader must be closed at once")
	}
	// Closed first, the reader's window would be None until the hold.
	if wantedAtClose != "3 4 5 6" {
		t.Fatalf("wanted when the reader closed: %q, want \"3 4 5 6\"", wantedAtClose)
	}
	if fp.String() != "3 4 5 6" || l.Active() != 1 || len(ts.pending) != 1 {
		t.Fatalf("after close: wanted %q active=%d timers=%d", fp, l.Active(), len(ts.pending))
	}
	if g := testutil.ToFloat64(promLingerWindows) - gauge; g != 1 {
		t.Fatalf("linger_windows moved by %v, want 1", g)
	}
	_ = r.Close() // double close (net/http may) must not hold twice
	if len(ts.pending) != 1 || l.Active() != 1 {
		t.Fatalf("double close: timers=%d active=%d", len(ts.pending), l.Active())
	}
	ts.fireAll()
	if fp.String() != "" || l.Active() != 0 {
		t.Fatalf("after linger: wanted %q active=%d", fp, l.Active())
	}
	if g := testutil.ToFloat64(promLingerWindows) - gauge; g != 0 {
		t.Fatalf("linger_windows off by %v after release", g)
	}
}

// A Seek (Range request) moves the window; the window never runs past the
// file's end.
func TestLinger_WindowFollowsSeekAndClipsToFile(t *testing.T) {
	l, _ := testLinger(time.Minute)
	fp := newFakePieces()
	r := l.wrap(&fakeReader{size: 10000}, fp, testWindow)
	if _, err := r.Seek(9000, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	// pos 9000 → offset 10000, file ends at 11000 → [10000, 11000) → pieces 9..10.
	if fp.String() != "9 10" {
		t.Fatalf("wanted %q, want \"9 10\"", fp)
	}
}

// Nothing left to want (client read to the end) → no window, no slot taken.
func TestLinger_NoWindowAtEOF(t *testing.T) {
	l, ts := testLinger(time.Minute)
	fp := newFakePieces()
	r := l.wrap(&fakeReader{size: 10000}, fp, testWindow)
	_, _ = io.Copy(io.Discard, r)
	_ = r.Close()
	if fp.String() != "" || l.Active() != 0 || len(ts.pending) != 0 {
		t.Fatalf("wanted %q active=%d timers=%d at EOF", fp, l.Active(), len(ts.pending))
	}
}

// A window with nothing left to download holds nothing and takes no slot:
// cached content must not fill the cap. A partly complete window holds the
// rest.
func TestLinger_CompletePiecesAreNotHeld(t *testing.T) {
	l, ts := testLinger(time.Minute)
	fp := newFakePieces(3, 4, 5, 6)
	r := l.wrap(&fakeReader{size: 10000}, fp, testWindow)
	_, _ = r.Seek(3000, io.SeekStart)
	_ = r.Close()
	if fp.String() != "" || l.Active() != 0 || len(ts.pending) != 0 {
		t.Fatalf("complete window: wanted %q active=%d timers=%d", fp, l.Active(), len(ts.pending))
	}
	// At the cap a complete window is not a rejection either.
	l.active = maxLingering
	r = l.wrap(&fakeReader{size: 10000}, fp, testWindow)
	_, _ = r.Seek(3000, io.SeekStart)
	_ = r.Close()
	if l.rejected != 0 {
		t.Fatalf("complete window counted as rejected at the cap: %d", l.rejected)
	}
	l.active = 0

	fp = newFakePieces(3, 4)
	r = l.wrap(&fakeReader{size: 10000}, fp, testWindow)
	_, _ = r.Seek(3000, io.SeekStart)
	_ = r.Close()
	if fp.String() != "5 6" || l.Active() != 1 {
		t.Fatalf("partly complete window: wanted %q active=%d", fp, l.Active())
	}
}

// Two requests that close on the same window (a download manager retrying
// one offset) each hold it: the first expiry must not drop the second's.
func TestLinger_OverlappingHoldsCount(t *testing.T) {
	l, ts := testLinger(time.Minute)
	fp := newFakePieces()
	for _, pos := range []int64{3000, 4000} {
		r := l.wrap(&fakeReader{size: 10000}, fp, testWindow)
		_, _ = r.Seek(pos, io.SeekStart)
		_ = r.Close()
	}
	// 3000 → pieces 3..6, 4000 → [5000, 7500) → pieces 4..7.
	if fp.String() != "3 4 5 6 7" || l.Active() != 2 {
		t.Fatalf("both held: wanted %q active=%d", fp, l.Active())
	}
	ts.fire(0)
	if fp.String() != "4 5 6 7" || l.Active() != 1 {
		t.Fatalf("first expired: wanted %q active=%d, want \"4 5 6 7\" active=1", fp, l.Active())
	}
	ts.fire(1)
	if fp.String() != "" || l.Active() != 0 {
		t.Fatalf("both expired: wanted %q active=%d", fp, l.Active())
	}
}

// Zero linger is the old behaviour: the reader itself, untouched.
func TestLinger_ZeroIsPassThrough(t *testing.T) {
	fr := &fakeReader{size: 10}
	if r := newLinger(0, 2500).wrap(fr, newFakePieces(), testWindow); r != io.ReadSeekCloser(fr) {
		t.Fatal("zero linger must return the reader unchanged")
	}
	var nilLinger *Linger
	if r := nilLinger.Wrap(fr, nil, nil); r != io.ReadSeekCloser(fr) {
		t.Fatal("nil linger must return the reader unchanged")
	}
}

// Past the cap a pod stops holding windows, just closes, and counts it.
func TestLinger_CapStopsHolding(t *testing.T) {
	l, _ := testLinger(time.Minute)
	l.active = maxLingering
	fr := &fakeReader{size: 10000}
	fp := newFakePieces()
	before := testutil.ToFloat64(promLingerRejected)
	_ = l.wrap(fr, fp, testWindow).Close()
	if fr.closed.Load() != 1 || fp.String() != "" || l.rejected != 1 {
		t.Fatalf("closed=%d wanted=%q rejected=%d", fr.closed.Load(), fp, l.rejected)
	}
	if d := testutil.ToFloat64(promLingerRejected) - before; d != 1 {
		t.Fatalf("linger_rejected_total moved by %v, want 1", d)
	}
}

// --- the real torrent ---

const lingerPL = 64 << 10

// lingerLeech is a torrent of 16 pieces of 64 KiB with no peers, in the
// service's storage (evictable when budget > 0), and a seeder holding it.
func lingerLeech(t *testing.T, budget int64) (*torrent.Torrent, *evictHook, *torrent.Client) {
	t.Helper()
	data, mi := multiChunkPayload(t, lingerPL, 16)
	seeder := seedingClient(t, data, mi)
	dir := t.TempDir()
	st := NewMMap(dir, budget, FileCacheConfig{})
	hook := &evictHook{ClientImpl: st, piece: -1, failed: make(chan struct{}), release: make(chan struct{})}
	cfg := addTestConfig(dir)
	cfg.DefaultStorage = hook
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	st.SetClient(cl)
	t.Cleanup(func() { cl.Close() })
	tor, err := cl.AddTorrent(mi)
	if err != nil {
		t.Fatal(err)
	}
	return tor, hook, seeder
}

// prios is the effective priority of pieces [0, n): "2 2 1 1 0 0".
func prios(tor *torrent.Torrent, n int) string {
	s := make([]string, n)
	for i := range s {
		s[i] = fmt.Sprint(int(tor.PieceState(i).Priority))
	}
	return strings.Join(s, " ")
}

// servedReader is a reader of file 0 set up as serveFile sets it, that has
// read once at pos: the Read returns at once, nothing being there.
func servedReader(tor *torrent.Torrent, pos int64) torrent.Reader {
	r := tor.Files()[0].NewReader()
	r.SetResponsive()
	r.SetReadaheadFunc(NewReadaheadFunc(4 * lingerPL))
	_, _ = r.Seek(pos, io.SeekStart)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	r.SetContext(ctx)
	_, _ = r.Read(make([]byte, 1024))
	return r
}

// What linger.go says of the library: an open reader that has read wants
// its window with no Read in flight, and Close drops it.
func TestLinger_LibraryOpenReaderKeepsWindow(t *testing.T) {
	tor, _, _ := lingerLeech(t, 0)
	r := servedReader(tor, 0)
	if got := prios(tor, 6); got != "5 3 3 3 0 0" {
		t.Fatalf("open reader after its Read returned: %s, want 5 3 3 3 0 0", got)
	}
	_ = r.Close()
	if got := prios(tor, 6); got != "0 0 0 0 0 0" {
		t.Fatalf("after Close: %s", got)
	}
}

// Closing the reader before holding would put its window at None for a
// moment: the piece leaves the pending set and peer requests for it are
// cancelled, the very thing the linger is for.
func TestLinger_WindowNeverPassesThroughNone(t *testing.T) {
	tor, _, _ := lingerLeech(t, 0)
	l, _ := testLinger(time.Minute)
	l.readahead = 4 * lingerPL
	lr := l.Wrap(servedReader(tor, 0), tor, tor.Files()[0])
	sub := tor.SubscribePieceStateChanges()
	defer sub.Close()
	_ = lr.Close()
	if got := prios(tor, 6); got != "1 1 1 1 0 0" {
		t.Fatalf("held window: %s, want 1 1 1 1 0 0", got)
	}
	for timeout := time.After(300 * time.Millisecond); ; {
		select {
		case c := <-sub.Values:
			if c.Index < 4 && c.Priority == torrent.PiecePriorityNone {
				t.Fatalf("piece %d went to None during Close", c.Index)
			}
			continue
		case <-timeout:
		}
		break
	}
}

// Warmup's High is left alone both ways: a window's hold does not lower it
// to Normal, and its release does not drop it to None.
func TestLinger_WarmupHighSurvivesAWindow(t *testing.T) {
	tor, _, _ := lingerLeech(t, 0)
	l, ts := testLinger(time.Minute)
	l.readahead = 4 * lingerPL
	(&Warmup{linger: l}).warm(tor, []int{0, 1, 2, 3})
	lr := l.Wrap(tor.Files()[0].NewReader(), tor, tor.Files()[0])
	_, _ = lr.Seek(2*lingerPL, io.SeekStart)
	_ = lr.Close()
	if got := prios(tor, 8); got != "2 2 2 2 1 1 0 0" {
		t.Fatalf("held: %s, want 2 2 2 2 1 1 0 0", got)
	}
	ts.fireAll()
	if got := prios(tor, 8); got != "2 2 2 2 0 0 0 0" {
		t.Fatalf("released: %s, want 2 2 2 2 0 0 0 0", got)
	}
}

// A piece that completes during the hold goes back to None at release.
// Left at Normal, it would be downloaded again, with nobody reading, the
// next time the cache evicts it.
func TestLinger_ReleaseClearsAPieceThatCompleted(t *testing.T) {
	tor, hook, seeder := lingerLeech(t, 16*lingerPL-1)
	l, ts := testLinger(time.Minute)
	l.readahead = 4 * lingerPL
	lr := l.Wrap(tor.Files()[0].NewReader(), tor, tor.Files()[0])
	_ = lr.Close()
	tor.AddClientPeer(seeder)
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if tor.PieceState(0).Complete && tor.PieceState(1).Complete && tor.PieceState(2).Complete && tor.PieceState(3).Complete {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("held window not downloaded: %v", tor.PieceStateRuns())
		}
	}
	// No source any more, so an evicted piece stays evicted whatever it wants.
	seeder.Torrents()[0].Drop()
	ts.fireAll()
	if !hook.ts.evictPiece(1) || tor.PieceState(1).Complete {
		t.Fatal("piece 1 not evicted")
	}
	if got := prios(tor, 6); got != "0 0 0 0 0 0" {
		t.Fatalf("after release and eviction: %s, want 0 0 0 0 0 0", got)
	}
}

// A reader on the window when the hold ends keeps its own priorities; once
// it moves away and closes, nothing holds the window.
func TestLinger_ReleaseUnderAnotherReader(t *testing.T) {
	tor, _, _ := lingerLeech(t, 0)
	l, ts := testLinger(time.Minute)
	l.readahead = 4 * lingerPL
	lr := l.Wrap(tor.Files()[0].NewReader(), tor, tor.Files()[0])
	_ = lr.Close()
	other := servedReader(tor, 0)
	ts.fireAll()
	if got := prios(tor, 6); got != "5 3 3 3 0 0" {
		t.Fatalf("released under a reader: %s, want 5 3 3 3 0 0", got)
	}
	_, _ = other.Seek(8*lingerPL, io.SeekStart)
	_ = other.Close()
	if got := prios(tor, 6); got != "0 0 0 0 0 0" {
		t.Fatalf("reader moved away and closed: %s, want 0 0 0 0 0 0", got)
	}
}

// A dropped torrent's counts go with it.
func TestLinger_ForgetsADroppedTorrent(t *testing.T) {
	tor, _, _ := lingerLeech(t, 0)
	l, ts := testLinger(time.Minute)
	l.readahead = 4 * lingerPL
	_ = l.Wrap(tor.Files()[0].NewReader(), tor, tor.Files()[0]).Close()
	tor.Drop()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		l.mu.Lock()
		n := len(l.holds)
		l.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("holds of a dropped torrent kept")
		}
	}
	ts.fireAll() // a timer that fires after the drop
	if l.Active() != 0 {
		t.Fatalf("active=%d after release", l.Active())
	}
}
