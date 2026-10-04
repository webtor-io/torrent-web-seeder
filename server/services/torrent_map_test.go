package services

import (
	"context"
	"os"
	"path/filepath"
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

// The clock availability_known runs on: since when a torrent has had
// connected peers without a break.
func TestPeerTimeline(t *testing.T) {
	var tl peerTimeline
	t0 := time.Unix(1000, 0)
	if d := tl.connectedFor(t0); d != 0 {
		t.Fatalf("no peer ever: %v, want 0", d)
	}
	tl.observe(2, t0)
	tl.observe(5, t0.Add(3*time.Second)) // more peers do not restart it
	if d := tl.connectedFor(t0.Add(25 * time.Second)); d != 25*time.Second {
		t.Fatalf("peers since t0: %v at t0+25s, want 25s", d)
	}
	tl.observe(0, t0.Add(26*time.Second))
	if d := tl.connectedFor(t0.Add(27 * time.Second)); d != 0 {
		t.Fatalf("after the last peer left: %v, want 0", d)
	}
	tl.observe(1, t0.Add(30*time.Second))
	if d := tl.connectedFor(t0.Add(31 * time.Second)); d != time.Second {
		t.Fatalf("a new peer after a break: %v, want 1s (the streak restarts)", d)
	}
	var none *peerTimeline
	if d := none.connectedFor(t0); d != 0 {
		t.Fatalf("untracked torrent: %v, want 0", d)
	}
}

// Get starts the torrent's watcher, which keeps the timeline: set once a
// peer connects, cleared when the last one goes.
func TestTorrentMap_GetKeepsPeerTimeline(t *testing.T) {
	data, mi := availabilityTestPayload(t)
	src := filepath.Join(t.TempDir(), "pack.torrent")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := mi.Write(f); err != nil {
		t.Fatal(err)
	}
	f.Close()
	cl := mmapClient(t, t.TempDir(), false)
	m := &TorrentMap{
		tc:      &TorrentClient{cl: cl, inited: true},
		fsm:     &FileStoreMap{p: src},
		entries: map[string]*torrentEntry{},
		ttl:     time.Minute,
	}
	h := mi.HashInfoBytes().HexString()
	tor, err := m.Get(context.Background(), h)
	if err != nil || tor == nil {
		t.Fatalf("Get: %v %v", tor, err)
	}
	t.Cleanup(func() {
		m.mux.Lock()
		defer m.mux.Unlock()
		m.entries[h].timer.Stop()
	})
	_, tl := m.peek(h)
	if tl == nil {
		t.Fatal("a torrent loaded by Get has no timeline")
	}
	if d := tl.connectedFor(time.Now()); d != 0 {
		t.Fatalf("no peer yet: connected for %v", d)
	}
	// A peer without the wanted piece: the connection stays up (see
	// swarmLeecher) until the peer goes.
	peer := swarmPeer(t, data, mi, 4*addTestPieceLen, 4)
	tor.DownloadPieces(8, 9)
	tor.AddClientPeer(peer)
	waitFor(t, "the timeline to start", func() bool { return tl.connectedFor(time.Now()) > 0 })
	peer.Close()
	waitFor(t, "the timeline to stop", func() bool { return tl.connectedFor(time.Now()) == 0 })
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
