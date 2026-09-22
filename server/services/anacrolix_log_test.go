package services

import (
	"log/slog"
	"testing"

	log "github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
)

// The library's reader failure lines reach logrus with their attributes as
// fields, group names flattened, and Info/Debug stay out.
func TestAnacrolixLogHandler_ForwardsWarnAndErrorWithFields(t *testing.T) {
	hook := test.NewGlobal()
	defer hook.Reset()
	prev := log.GetLevel()
	log.SetLevel(log.DebugLevel)
	defer log.SetLevel(prev)

	l := slog.New(newAnacrolixLogHandler(slog.LevelWarn)).
		With(slog.Group("torrent", "ih", "deadbeef")).
		WithGroup("req")
	l.Error("initial read failed", "err", "piece evicted from cache", "piece", 1869)
	l.Warn("something odd", "n", 0)
	l.Info("chatter")
	l.Debug("more chatter")

	if len(hook.Entries) != 2 {
		t.Fatalf("entries = %d, want 2 (Info/Debug must be dropped): %+v", len(hook.Entries), hook.Entries)
	}
	e := hook.Entries[0]
	if e.Level != log.ErrorLevel || e.Message != "initial read failed" {
		t.Fatalf("first entry = %v %q", e.Level, e.Message)
	}
	for k, want := range map[string]any{"torrent.ih": "deadbeef", "req.err": "piece evicted from cache", "req.piece": int64(1869), "source": "anacrolix"} {
		if got := e.Data[k]; got != want {
			t.Errorf("field %s = %#v, want %#v", k, got, want)
		}
	}
	if hook.Entries[1].Level != log.WarnLevel {
		t.Errorf("second entry level = %v, want warn", hook.Entries[1].Level)
	}
}

// A panic recovered by the HTTP middleware is counted, so the rate can be
// alerted on without Loki.
func TestRecoverMiddleware_CountsPanics(t *testing.T) {
	before := counterValue(t, promRecoveredPanics)
	rec := newTestResponseRecorder()
	RecoverMiddleware(panickingHandler{}).ServeHTTP(rec, newTestRequest())
	if rec.status != 500 {
		t.Fatalf("status = %d, want 500", rec.status)
	}
	if got := counterValue(t, promRecoveredPanics) - before; got != 1 {
		t.Fatalf("panics counted = %v, want 1", got)
	}
}
