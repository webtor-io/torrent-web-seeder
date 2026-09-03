package services

import (
	"io"
	"sync/atomic"
	"testing"
	"time"
)

type fakeReader struct {
	closed atomic.Int32
	pos    int64
	size   int64
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
func (f *fakeReader) Close() error { f.closed.Add(1); return nil }

type recorder struct {
	raised, released [][2]int
}

func (r *recorder) raise(b, e int)   { r.raised = append(r.raised, [2]int{b, e}) }
func (r *recorder) release(b, e int) { r.released = append(r.released, [2]int{b, e}) }

func manualTimers() (fire func(), after func(time.Duration, func()) *time.Timer) {
	var pending []func()
	after = func(_ time.Duration, fn func()) *time.Timer {
		pending = append(pending, fn)
		return nil
	}
	fire = func() {
		for _, fn := range pending {
			fn()
		}
		pending = nil
	}
	return
}

// File starts 1000 bytes into the torrent, 10 000 bytes long, 1 KiB
// pieces (so it spans pieces 0..10), readahead 2500 bytes.
var testWindow = window{fileOffset: 1000, fileLength: 10000, pieceLen: 1024}

// Close must raise the pieces of [pos, pos+readahead) at once and release
// exactly that range when the linger elapses; the torrent reader itself is
// closed immediately — an idle reader holds no priorities, the explicit
// piece priorities do that job.
func TestLinger_RaisesWindowOnCloseAndReleasesLater(t *testing.T) {
	l := newLinger(90*time.Second, 2500)
	fire, after := manualTimers()
	l.afterFn = after
	fr := &fakeReader{size: 10000}
	rec := &recorder{}
	r := l.wrap(fr, rec, testWindow)
	// The client read 3000 bytes then dropped: pos = 3000 → torrent offset
	// 4000, window [4000, 6500) → pieces 3..6 (6500-1)/1024 = 6 → end 7.
	if _, err := io.CopyN(io.Discard, r, 3000); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if fr.closed.Load() != 1 {
		t.Fatal("underlying reader must be closed at once")
	}
	if len(rec.raised) != 1 || rec.raised[0] != [2]int{3, 7} {
		t.Fatalf("raised %v, want [[3 7]]", rec.raised)
	}
	if len(rec.released) != 0 || l.Active() != 1 {
		t.Fatalf("released early: %v active=%d", rec.released, l.Active())
	}
	_ = r.Close() // double close (net/http may) must not raise twice
	fire()
	if len(rec.released) != 1 || rec.released[0] != [2]int{3, 7} || len(rec.raised) != 1 {
		t.Fatalf("after linger: raised %v released %v", rec.raised, rec.released)
	}
	if l.Active() != 0 {
		t.Fatalf("active=%d after linger, want 0", l.Active())
	}
}

// A Seek (Range request) moves the window; the window never runs past the
// file's end.
func TestLinger_WindowFollowsSeekAndClipsToFile(t *testing.T) {
	l := newLinger(time.Minute, 2500)
	_, after := manualTimers()
	l.afterFn = after
	fr := &fakeReader{size: 10000}
	rec := &recorder{}
	r := l.wrap(fr, rec, testWindow)
	if _, err := r.Seek(9000, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	// pos 9000 → offset 10000, file ends at 11000 → [10000, 11000) → pieces 9..10 → end 11.
	if len(rec.raised) != 1 || rec.raised[0] != [2]int{9, 11} {
		t.Fatalf("raised %v, want [[9 11]]", rec.raised)
	}
}

// Nothing left to want (client read to the end) → no window, no slot taken.
func TestLinger_NoWindowAtEOF(t *testing.T) {
	l := newLinger(time.Minute, 2500)
	fr := &fakeReader{size: 10000}
	rec := &recorder{}
	r := l.wrap(fr, rec, testWindow)
	_, _ = io.Copy(io.Discard, r)
	_ = r.Close()
	if len(rec.raised) != 0 || l.Active() != 0 {
		t.Fatalf("raised %v active=%d at EOF", rec.raised, l.Active())
	}
}

// Zero linger is the old behaviour: the reader itself, untouched.
func TestLinger_ZeroIsPassThrough(t *testing.T) {
	fr := &fakeReader{size: 10}
	if r := newLinger(0, 2500).wrap(fr, &recorder{}, testWindow); r != io.ReadSeekCloser(fr) {
		t.Fatal("zero linger must return the reader unchanged")
	}
	var nilLinger *Linger
	if r := nilLinger.Wrap(fr, nil, nil); r != io.ReadSeekCloser(fr) {
		t.Fatal("nil linger must return the reader unchanged")
	}
}

// Past the cap a pod stops raising windows and just closes.
func TestLinger_CapStopsRaising(t *testing.T) {
	l := newLinger(time.Minute, 2500)
	_, after := manualTimers()
	l.afterFn = after
	l.active.Store(maxLingering)
	fr := &fakeReader{size: 10000}
	rec := &recorder{}
	_ = l.wrap(fr, rec, testWindow).Close()
	if fr.closed.Load() != 1 || len(rec.raised) != 0 || l.rejected.Load() != 1 {
		t.Fatalf("closed=%d raised=%v rejected=%d", fr.closed.Load(), rec.raised, l.rejected.Load())
	}
}
