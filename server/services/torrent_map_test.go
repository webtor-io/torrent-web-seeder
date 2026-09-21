package services

import (
	"sync/atomic"
	"testing"
	"time"
)

// A torrent being served is not dropped when its TTL fires; it is dropped
// one TTL after the last request released it. A torrent nobody holds still
// expires on time, exactly once.
func TestTorrentMap_HoldDefersDrop(t *testing.T) {
	m := &TorrentMap{entries: map[string]*torrentEntry{}, ttl: 40 * time.Millisecond}
	var drops atomic.Int32
	m.mux.Lock()
	m.track("held", func() { drops.Add(1) })
	m.track("idle", func() { drops.Add(1) })
	m.mux.Unlock()

	release := m.Hold("held")
	time.Sleep(120 * time.Millisecond)
	if n := drops.Load(); n != 1 {
		t.Fatalf("drops = %d after 3 TTLs, want 1 (only the idle torrent)", n)
	}
	m.mux.Lock()
	_, heldOK := m.entries["held"]
	_, idleOK := m.entries["idle"]
	m.mux.Unlock()
	if !heldOK || idleOK {
		t.Fatalf("held=%v idle=%v, want held kept and idle gone", heldOK, idleOK)
	}

	release()
	release() // idempotent
	time.Sleep(25 * time.Millisecond)
	if n := drops.Load(); n != 1 {
		t.Fatalf("drops = %d right after release, want 1 (a full TTL must pass first)", n)
	}
	time.Sleep(60 * time.Millisecond)
	if n := drops.Load(); n != 2 {
		t.Fatalf("drops = %d one TTL after release, want 2", n)
	}
	if m.Hold("gone") == nil {
		t.Fatal("Hold on an unknown torrent must return a no-op release")
	}
}
