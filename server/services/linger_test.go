package services

import (
	"io"
	"sync/atomic"
	"testing"
	"time"
)

type fakeReader struct {
	closed atomic.Int32
}

func (f *fakeReader) Read(p []byte) (int, error)         { return 0, io.EOF }
func (f *fakeReader) Seek(o int64, w int) (int64, error) { return 0, nil }
func (f *fakeReader) Close() error                       { f.closed.Add(1); return nil }
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

// Close on a wrapped reader must not close the torrent reader until the
// linger elapses; the piece priorities it implies stay up for that long.
func TestLinger_DefersClose(t *testing.T) {
	l := newLinger(90 * time.Second)
	fire, after := manualTimers()
	l.afterFn = after
	fr := &fakeReader{}
	r := l.Wrap(fr)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if fr.closed.Load() != 0 {
		t.Fatal("reader closed immediately — linger did nothing")
	}
	if l.Active() != 1 {
		t.Fatalf("active=%d, want 1", l.Active())
	}
	_ = r.Close() // net/http may close twice; must stay a single deferred close
	fire()
	if fr.closed.Load() != 1 {
		t.Fatalf("closed %d times after linger, want exactly 1", fr.closed.Load())
	}
	if l.Active() != 0 {
		t.Fatalf("active=%d after linger, want 0", l.Active())
	}
}

// Zero linger is the old behaviour: the reader itself, closed at once.
func TestLinger_ZeroIsPassThrough(t *testing.T) {
	l := newLinger(0)
	fr := &fakeReader{}
	if r := l.Wrap(fr); r != io.ReadSeekCloser(fr) {
		t.Fatal("zero linger must return the reader unchanged")
	}
	var nilLinger *Linger
	if r := nilLinger.Wrap(fr); r != io.ReadSeekCloser(fr) {
		t.Fatal("nil linger must return the reader unchanged")
	}
}

// Past the cap a pod stops holding readers and closes immediately.
func TestLinger_CapClosesImmediately(t *testing.T) {
	l := newLinger(time.Minute)
	_, after := manualTimers()
	l.afterFn = after
	l.active.Store(maxLingering)
	fr := &fakeReader{}
	_ = l.Wrap(fr).Close()
	if fr.closed.Load() != 1 {
		t.Fatal("at the cap the reader must close immediately")
	}
	if l.rejected.Load() != 1 || l.Active() != maxLingering {
		t.Fatalf("rejected=%d active=%d", l.rejected.Load(), l.Active())
	}
}
