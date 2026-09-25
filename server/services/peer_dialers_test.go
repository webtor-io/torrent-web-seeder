package services

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
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

// A dial round opens one TCP connection to a peer. The library dials through
// every registered dialer at once, so a second TCP dialer opens a second
// connection to every peer: the plain "tcp" metricsDialer did, and so would
// the client's own sockets if they were registered unwrapped next to the
// wrapped ones (cfg.DialForPeerConns left on), with those dials uncounted.
func TestNewPeerClient_OneTCPConnectionPerDial(t *testing.T) {
	_, mi := addTestPayload(t)

	// A peer that accepts and never answers, so the first dial round stays
	// open for the handshake timeout and every dialer's connection lands.
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var (
		accepted atomic.Int32
		mu       sync.Mutex
		conns    []net.Conn
	)
	first := make(chan struct{}, 1)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			accepted.Add(1)
			select {
			case first <- struct{}{}:
			default:
			}
		}
	}()

	cfg := addTestConfig(t.TempDir())
	cfg.DisableUTP = false
	cfg.HandshakesTimeout = 5 * time.Second
	cl, err := newPeerClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Cleanups run last-in first-out: the peer hangs up first, which fails
	// the pending handshake at once instead of after the timeout.
	t.Cleanup(func() { cl.Close() })
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	tor, err := cl.AddTorrent(mi)
	if err != nil {
		t.Fatal(err)
	}
	r := tor.NewReader()
	t.Cleanup(func() { _ = r.Close() })
	go func() { _, _ = io.Copy(io.Discard, r) }()

	tor.AddPeers([]torrent.PeerInfo{{Addr: ln.Addr(), Trusted: true}})
	select {
	case <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("the peer was never dialed")
	}
	// Loopback connects take microseconds; the round's other dials have
	// landed long before this, and the handshake timeout is far off.
	time.Sleep(300 * time.Millisecond)
	if n := accepted.Load(); n != 1 {
		t.Errorf("%d TCP connections to the peer in one dial round, want 1", n)
	}
}
