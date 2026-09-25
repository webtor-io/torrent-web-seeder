package services

import (
	"bufio"
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	pp "github.com/anacrolix/torrent/peer_protocol"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// chokingPeer has every piece and serves each new connection budget chunks,
// then chokes it for good: the lone peer of 2026-09-25, which served a fresh
// connection for seconds and never again.
type chokingPeer struct {
	ln       net.Listener
	ih       metainfo.Hash
	data     []byte
	pieceLen int
	budget   int
	conns    atomic.Int32
}

func newChokingPeer(t *testing.T, data []byte, mi *metainfo.MetaInfo, budget int) *chokingPeer {
	t.Helper()
	info, err := mi.UnmarshalInfo()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &chokingPeer{ln: ln, ih: mi.HashInfoBytes(), data: data, pieceLen: int(info.PieceLength), budget: budget}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			p.conns.Add(1)
			go p.handle(c)
		}
	}()
	return p
}

func (p *chokingPeer) handle(c net.Conn) {
	defer c.Close()
	var id [20]byte
	copy(id[:], "-CP0001-chokingpeer0")
	var ext pp.PeerExtensionBits
	ext.SetBit(pp.ExtensionBitFast, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pp.Handshake(ctx, c, &p.ih, id, ext); err != nil {
		return
	}
	w := bufio.NewWriter(c)
	write := func(m pp.Message) error {
		b, err := m.MarshalBinary()
		if err != nil {
			return err
		}
		if _, err := w.Write(b); err != nil {
			return err
		}
		return w.Flush()
	}
	if write(pp.Message{Type: pp.HaveAll}) != nil || write(pp.Message{Type: pp.Unchoke}) != nil {
		return
	}
	d := pp.Decoder{
		R:         bufio.NewReader(c),
		MaxLength: 256 << 10,
		Pool:      &sync.Pool{New: func() any { b := make([]byte, 16<<10); return &b }},
	}
	served, choked := 0, false
	for {
		var m pp.Message
		if err := d.Decode(&m); err != nil {
			return
		}
		if m.Type != pp.Request {
			continue
		}
		if served >= p.budget {
			if !choked && write(pp.Message{Type: pp.Choke}) != nil {
				return
			}
			choked = true
			continue
		}
		off := int(m.Index)*p.pieceLen + int(m.Begin)
		if write(pp.Message{Type: pp.Piece, Index: m.Index, Begin: m.Begin, Piece: p.data[off : off+int(m.Length)]}) != nil {
			return
		}
		served++
	}
}

var testChokeRedial = chokeRedial{
	tick:     50 * time.Millisecond,
	stall:    200 * time.Millisecond,
	choked:   200 * time.Millisecond,
	cooldown: 300 * time.Millisecond,
	perTick:  5,
}

// A stalled read gets past a peer that chokes every connection after two
// chunks: the watcher keeps reconnecting it. Without the watcher the read
// stops at the second piece.
func TestChokeRedial_ReadsPastAPeerThatChokes(t *testing.T) {
	data, mi := addTestPayload(t)
	peer := newChokingPeer(t, data, mi, 2)
	tor, err := addTorrent(mmapClient(t, t.TempDir(), false), mi)
	if err != nil {
		t.Fatal(err)
	}
	go testChokeRedial.watch(tor, func() bool { return true })
	tor.AddPeers([]torrent.PeerInfo{{Addr: peer.ln.Addr(), Trusted: true}})
	readAllWithin(t, tor, data, 20*time.Second)
	if n := peer.conns.Load(); n < 2 {
		t.Errorf("the peer saw %d connections, want redials", n)
	}
}

// A torrent nobody is reading is not redialled, however long it is choked.
func TestChokeRedial_LeavesAnUnreadTorrentAlone(t *testing.T) {
	data, mi := addTestPayload(t)
	peer := newChokingPeer(t, data, mi, 2)
	tor, err := addTorrent(mmapClient(t, t.TempDir(), false), mi)
	if err != nil {
		t.Fatal(err)
	}
	before := testutil.ToFloat64(promChokedRedials)
	go testChokeRedial.watch(tor, func() bool { return false })
	tor.AddPeers([]torrent.PeerInfo{{Addr: peer.ln.Addr(), Trusted: true}})
	tor.DownloadAll()
	time.Sleep(2 * time.Second)
	if n := testutil.ToFloat64(promChokedRedials) - before; n != 0 {
		t.Errorf("%v redials of a torrent nobody reads, want 0", n)
	}
}

// A peer that never unchokes is redialled at most once per cooldown, not on
// every tick.
func TestChokeRedial_CooldownPerPeer(t *testing.T) {
	data, mi := addTestPayload(t)
	peer := newChokingPeer(t, data, mi, 0)
	tor, err := addTorrent(mmapClient(t, t.TempDir(), false), mi)
	if err != nil {
		t.Fatal(err)
	}
	c := testChokeRedial
	c.cooldown = time.Second
	before := testutil.ToFloat64(promChokedRedials)
	go c.watch(tor, func() bool { return true })
	tor.AddPeers([]torrent.PeerInfo{{Addr: peer.ln.Addr(), Trusted: true}})
	tor.DownloadAll()
	const window = 2 * time.Second
	time.Sleep(window)
	n := testutil.ToFloat64(promChokedRedials) - before
	if limit := float64(window/c.cooldown) + 1; n < 1 || n > limit {
		t.Errorf("%v redials in %v, want 1..%v (one per %v)", n, window, limit, c.cooldown)
	}
}
