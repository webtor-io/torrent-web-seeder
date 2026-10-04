package services

import (
	"fmt"

	g "github.com/anacrolix/generics"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/RoaringBitmap/roaring"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

// BenchmarkStatCost measures the glue against the library itself: a leecher
// in this service's storage with real connected peers, each claiming a
// random half of a torrent of `pieces` pieces, and the calls a stat makes.
//
//   - readSwarm: what this change adds when no seeder is connected —
//     WebseedPeerConns, PeerConns, and per connection PeerPieces (read lock
//   - bitmap clone) folded into one union. Divide by the connection count
//     (ns/conn) for the per-peer cost. Once per torrent per second.
//   - computeAvailability for the whole torrent and for a 200-piece file on
//     what readSwarm returned. Once per path per refresh.
//   - pieceStateLoop: what torrentStat already did before this change, one
//     Piece.State (one read lock) per piece, for comparison.
//   - pieceStateRuns: the same states under one read lock.
//
// The peers claim their pieces through a pre-filled completion, so nothing
// is written or hashed; the leecher wants only the last piece, which no peer
// has, so the connections stay up and nothing is requested. Run it at the
// default benchtime: allocations are counted process-wide, and a short run
// also counts the swarm's own settling.
func BenchmarkStatCost(b *testing.B) {
	for _, c := range []struct{ pieces, peers int }{{8 << 10, 16}, {64 << 10, 16}} {
		b.Run(fmt.Sprintf("pieces=%d/peers=%d", c.pieces, c.peers), func(b *testing.B) {
			tor := benchSwarm(b, c.pieces, c.peers)
			b.Run("readSwarm", func(b *testing.B) {
				b.ReportAllocs()
				conns := 0
				for i := 0; i < b.N; i++ {
					conns += readSwarm(tor, nil).connected
				}
				b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(conns), "ns/conn")
				b.ReportMetric(float64(conns)/float64(b.N), "conns")
			})
			sw := readSwarm(tor, nil)
			in := availabilityInput{complete: make([]bool, c.pieces), wanted: make([]bool, c.pieces), peers: []*roaring.Bitmap{sw.union}, connected: sw.connected, peersFor: time.Minute}
			b.Run("computeAvailability/scope=torrent", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					computeAvailability(in)
				}
			})
			file := fileScope(in, c.pieces/2, 200)
			b.Run("computeAvailability/scope=file200", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					computeAvailability(file)
				}
			})
			b.Run("pieceStateLoop", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					for p := 0; p < c.pieces; p++ {
						_ = tor.Piece(p).State()
					}
				}
				b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*c.pieces), "ns/piece")
			})
			b.Run("pieceStateRuns", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					_ = tor.PieceStateRuns()
				}
			})
		})
	}
}

func benchSwarm(b *testing.B, pieces, peers int) *torrent.Torrent {
	b.Helper()
	const pl = 16 << 10
	infoBytes, err := bencode.Marshal(metainfo.Info{
		Name:        "bench.bin",
		PieceLength: pl,
		Pieces:      make([]byte, 20*pieces),
		Length:      int64(pieces) * pl,
	})
	if err != nil {
		b.Fatal(err)
	}
	mi := &metainfo.MetaInfo{InfoBytes: infoBytes}
	ih := mi.HashInfoBytes()
	r := rand.New(rand.NewPCG(3, 4))
	var peerClients []*torrent.Client
	for p := 0; p < peers; p++ {
		dir := b.TempDir()
		// File storage reports a piece incomplete when its file is missing
		// or short, whatever the completion says: a sparse file of the
		// torrent's length, nothing written.
		f, err := os.Create(filepath.Join(dir, "bench.bin"))
		if err != nil {
			b.Fatal(err)
		}
		if err := f.Truncate(int64(pieces) * pl); err != nil {
			b.Fatal(err)
		}
		f.Close()
		pc := storage.NewMapPieceCompletion()
		for i := 0; i < pieces; i++ {
			_ = pc.Set(metainfo.PieceKey{InfoHash: ih, Index: i}, i < pieces-1 && r.IntN(2) == 0)
		}
		cfg := addTestConfig(dir)
		// A client that neither needs data nor seeds drops connections as
		// useless.
		cfg.Seed = true
		// Without part files: with them the storage marks every piece of a
		// file present under its final name complete.
		st := storage.NewFileOpts(storage.NewFileClientOpts{ClientBaseDir: dir, PieceCompletion: pc, UsePartFiles: g.Some(false)})
		cfg.DefaultStorage = st
		cl, err := torrent.NewClient(cfg)
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() {
			cl.Close()
			_ = st.Close()
		})
		if _, err := cl.AddTorrent(mi); err != nil {
			b.Fatal(err)
		}
		peerClients = append(peerClients, cl)
	}
	cl := benchMmapClient(b)
	tor, err := addTorrent(cl, mi)
	if err != nil {
		b.Fatal(err)
	}
	tor.DownloadPieces(pieces-1, pieces)
	for _, pc := range peerClients {
		tor.AddClientPeer(pc)
	}
	// Ready when every peer — by peer ID, not by connection — has sent its
	// pieces. A pair may hold two connections, one each way, until the
	// library drops the duplicate, so counting connections could pass 16
	// and never equal it; the per-connection metric counts what is there.
	deadline := time.Now().Add(30 * time.Second)
	for {
		ready := map[torrent.PeerID]struct{}{}
		for _, c := range tor.PeerConns() {
			if c.PeerPieces().GetCardinality() > 0 {
				ready[c.PeerID] = struct{}{}
			}
		}
		if len(ready) >= peers {
			break
		}
		if time.Now().After(deadline) {
			s := tor.Stats()
			b.Fatalf("%d of %d peers connected with their pieces; conns=%d active=%d halfopen=%d pending=%d runs=%v", len(ready), peers, len(tor.PeerConns()), s.ActivePeers, s.HalfOpenPeers, s.PendingPeers, tor.PieceStateRuns())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if s := tor.Stats(); s.ConnectedSeeders != 0 || s.ActivePeers < peers {
		b.Fatalf("seeders=%d active=%d, want 0/>=%d", s.ConnectedSeeders, s.ActivePeers, peers)
	}
	// Let the connecting settle (duplicates dropped, bitfields and keepalives
	// out) before anything is measured.
	time.Sleep(time.Second)
	return tor
}

func benchMmapClient(b *testing.B) *torrent.Client {
	b.Helper()
	dir := b.TempDir()
	cfg := addTestConfig(dir)
	cfg.DefaultStorage = NewMMap(dir, 0, FileCacheConfig{})
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { cl.Close() })
	return cl
}
