package services

import (
	"context"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
)

// A stream with no progress is cancelled after the timeout; one that keeps
// writing is not; a disabled timeout never cancels.
func TestWatchStall(t *testing.T) {
	logger := log.WithField("test", "stall")

	// No progress at all: cancelled once idle > timeout.
	ctx, cancel := context.WithCancel(context.Background())
	go watchStall(ctx, cancel, func() time.Time { return time.Time{} }, 60*time.Millisecond, logger)
	select {
	case <-ctx.Done():
	case <-time.After(400 * time.Millisecond):
		t.Fatal("stalled stream was not cancelled")
	}

	// Progress keeps it alive.
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	stop := make(chan struct{})
	go func() {
		tk := time.NewTicker(10 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				last.Store(time.Now().UnixNano())
			}
		}
	}()
	go watchStall(ctx2, cancel2, func() time.Time { return time.Unix(0, last.Load()) }, 60*time.Millisecond, logger)
	time.Sleep(250 * time.Millisecond)
	close(stop)
	if ctx2.Err() != nil {
		t.Fatal("a progressing stream must not be cancelled")
	}

	// Disabled.
	ctx3, cancel3 := context.WithCancel(context.Background())
	defer cancel3()
	go watchStall(ctx3, cancel3, func() time.Time { return time.Time{} }, 0, logger)
	time.Sleep(100 * time.Millisecond)
	if ctx3.Err() != nil {
		t.Fatal("timeout 0 must disable the guard")
	}
}

// TouchWriter records the time of its last write.
func TestTouchWriter_LastWrite(t *testing.T) {
	tm := &TorrentMap{entries: map[string]*torrentEntry{}, ttl: time.Second}
	w := NewTouchWriter(httptest.NewRecorder(), tm, "h")
	if !w.LastWrite().IsZero() {
		t.Fatal("no write yet must read as zero time")
	}
	before := time.Now()
	if _, err := w.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if w.LastWrite().Before(before) {
		t.Fatal("LastWrite must be updated by Write")
	}
}
