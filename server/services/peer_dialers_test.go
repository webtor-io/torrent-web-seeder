package services

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/storage"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// A peer listening on TCP and uTP, as clients do, is handshaken with well
// inside the handshake timeout, and the dial is counted. A dialer that
// returns a connection no peer speaks on, such as a bare UDP socket, wins the
// library's dial race and makes every connection wait out the timeout first.
func TestNewPeerClient_HandshakesBeforeTimeoutAndCountsDials(t *testing.T) {
	const handshakeTimeout = 2 * time.Second
	data, mi := addTestPayload(t)

	// A seeding peer: one without data or demand refuses connections.
	pdir := t.TempDir()
	if err := os.WriteFile(filepath.Join(pdir, "payload.bin"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	pcfg := addTestConfig(pdir)
	pcfg.DisableUTP = false
	pcfg.Seed = true
	st := storage.NewFileOpts(storage.NewFileClientOpts{
		ClientBaseDir:   pdir,
		PieceCompletion: storage.NewMapPieceCompletion(),
		UsePartFiles:    g.Some(false),
	})
	pcfg.DefaultStorage = st
	peer, err := torrent.NewClient(pcfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { peer.Close(); _ = st.Close() })
	if _, err := peer.AddTorrent(mi); err != nil {
		t.Fatal(err)
	}

	cfg := addTestConfig(t.TempDir())
	cfg.DisableUTP = false
	cfg.HandshakesTimeout = handshakeTimeout
	handshook := make(chan string, 1)
	cfg.Callbacks.CompletedHandshake = func(pc *torrent.PeerConn, _ torrent.InfoHash) {
		select {
		case handshook <- pc.Network:
		default:
		}
	}
	cl, err := newPeerClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })
	tor, err := cl.AddTorrent(mi)
	if err != nil {
		t.Fatal(err)
	}

	// Without demand the torrent opens no connections.
	r := tor.NewReader()
	t.Cleanup(func() { _ = r.Close() })
	go func() { _, _ = io.Copy(io.Discard, r) }()

	dials := testutil.ToFloat64(promDialAttempts.WithLabelValues("peer"))
	start := time.Now()
	tor.AddClientPeer(peer)
	var network string
	select {
	case network = <-handshook:
	case <-time.After(5 * handshakeTimeout):
		t.Fatal("no handshake with the peer")
	}
	d := time.Since(start)
	t.Logf("first handshake over %s after %v", network, d)
	if d > handshakeTimeout/2 {
		t.Errorf("first handshake (%s) after %v, handshake timeout %v", network, d, handshakeTimeout)
	}
	if n := testutil.ToFloat64(promDialAttempts.WithLabelValues("peer")) - dials; n < 1 {
		t.Errorf("%v peer dials counted", n)
	}
}
