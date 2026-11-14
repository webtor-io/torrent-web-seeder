package services

import (
	"bytes"
	"context"
	"flag"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urfave/cli"
)

// openShared opens info in dir as a Client's storage would, registered in sib.
func openShared(t *testing.T, dir string, sib *storageSiblings, info *metainfo.Info, ih metainfo.Hash) storage.TorrentImpl {
	t.Helper()
	m := NewMMap(dir, 0, FileCacheConfig{})
	m.siblings = sib
	ts, err := m.OpenTorrent(context.Background(), info, ih)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ts.Close() })
	return ts
}

// podClients builds the pod's Client and an account's Client on one data dir
// the way the service does: NewTorrentClient makes the registry, forUser shares
// it and get hands it to each Client's storage. Nothing here sets it by hand.
func podClients(t *testing.T) (pod, account *torrent.Client) {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String(DataDirFlag, t.TempDir(), "")
	tc, err := NewTorrentClient(cli.NewContext(nil, fs, nil))
	if err != nil {
		t.Fatal(err)
	}
	tc.swarm = newSwarmStats() // the service's collector is process-wide
	tc.testConfig = loopbackConfig
	var cls []*torrent.Client
	for _, cc := range []*TorrentClient{tc, tc.forUser()} {
		cl, err := cc.get()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cl.Close() })
		cls = append(cls, cl)
	}
	return cls[0], cls[1]
}

// A chunk written over a piece the other Client of this pod has verified is dropped
// and reported written; a piece nobody completed is written. A nil registry is
// another pod: its write lands, as it does today.
func TestSiblings_WriteGuardKeepsCompletePiece(t *testing.T) {
	for _, c := range []struct {
		name    string
		sib     *storageSiblings
		guarded bool
	}{{"same pod", newStorageSiblings(), true}, {"another pod", nil, false}} {
		t.Run(c.name, func(t *testing.T) {
			data, mi := addTestPayload(t)
			info, _ := mi.UnmarshalInfo()
			ih, dir := mi.HashInfoBytes(), t.TempDir()
			a, b := openShared(t, dir, c.sib, &info, ih), openShared(t, dir, c.sib, &info, ih)
			p0, p1 := info.Piece(0), info.Piece(1)
			if _, err := a.Piece(p0).WriteAt(data[:addTestPieceLen], 0); err != nil {
				t.Fatal(err)
			}
			if err := a.Piece(p0).MarkComplete(); err != nil {
				t.Fatal(err)
			}
			dropped := testutil.ToFloat64(promSharedChunkWritesDropped)
			bad := bytes.Repeat([]byte{0xff}, 1024)
			if n, err := b.Piece(p0).WriteAt(bad, 0); n != len(bad) || err != nil {
				t.Fatalf("WriteAt over a complete piece = (%d, %v), want (%d, nil)", n, err, len(bad))
			}
			got := make([]byte, 1024)
			if _, err := a.Piece(p0).ReadAt(got, 0); err != nil {
				t.Fatal(err)
			}
			if want := map[bool][]byte{true: data[:1024], false: bad}[c.guarded]; !bytes.Equal(got, want) {
				t.Fatalf("guarded=%v: piece 0 starts %x, want %x", c.guarded, got[:4], want[:4])
			}
			if d := testutil.ToFloat64(promSharedChunkWritesDropped) - dropped; d != map[bool]float64{true: 1, false: 0}[c.guarded] {
				t.Fatalf("dropped +%v", d)
			}
			chunk := data[addTestPieceLen : addTestPieceLen+1024]
			if _, err := b.Piece(p1).WriteAt(chunk, 0); err != nil {
				t.Fatal(err)
			}
			if _, err := b.Piece(p1).ReadAt(got, 0); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, chunk) {
				t.Fatal("a piece nobody completed was not written")
			}
		})
	}
}

// B, an account's Client, holds the torrent with no peer and a reader waiting;
// A, the pod's Client, downloads it. B has no peer, so every byte its reader
// gets comes from A's verified pieces.
func TestSiblings_ReaderGetsSiblingPiece(t *testing.T) {
	data, mi := addTestPayload(t)
	a, b := podClients(t)
	tb, err := addTorrent(b, mi)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan []byte, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		r := tb.NewReader()
		r.SetContext(ctx)
		buf, _ := io.ReadAll(r)
		got <- buf
	}()
	ta, err := addTorrent(a, mi)
	if err != nil {
		t.Fatal(err)
	}
	ta.DownloadAll()
	ta.AddClientPeer(seedingClient(t, data, mi))
	if buf := <-got; !bytes.Equal(buf, data) {
		t.Fatalf("B read %d bytes, want the %d of the payload", len(buf), len(data))
	}
}

// A burst of pieces verified by one Client reaches the other through one
// worker at a time, each drained batch in one call of the library's batch
// refresh, and every piece of the burst arrives. A goroutine per piece, each
// taking the other Client's lock, kept that Client's own requests waiting
// behind the queue while its sibling downloaded; a lock and a new-conns pass
// per piece of a batch kept it pieces behind.
func TestSiblings_BurstRefreshesThroughOneWorker(t *testing.T) {
	_, mi := addTestPayload(t)
	a, b := podClients(t)
	tb, err := addTorrent(b, mi)
	if err != nil {
		t.Fatal(err)
	}
	ta, err := addTorrent(a, mi)
	if err != nil {
		t.Fatal(err)
	}
	bs := tb.Piece(0).Storage().PieceImpl.(mmapStoragePiece).t
	var running, most, drains, calls, pieces atomic.Int32
	burst := make(chan struct{})
	bs.refreshHook = func() {
		drains.Add(1)
		n := running.Add(1)
		for m := most.Load(); n > m && !most.CompareAndSwap(m, n); m = most.Load() {
		}
		<-burst // the rest of the burst piles up behind the first drain
		running.Add(-1)
	}
	bs.refreshPieces = func(tor *torrent.Torrent, idx []int) int {
		calls.Add(1)
		pieces.Add(int32(len(idx)))
		return tor.RefreshCompletionsFromStorage(idx)
	}
	for i := range ta.NumPieces() {
		if err := ta.Piece(i).Storage().MarkComplete(); err != nil {
			t.Fatal(err)
		}
	}
	close(burst)
	waitFor(t, "B to see every piece complete", func() bool {
		for i := range tb.NumPieces() {
			if !tb.Piece(i).State().Complete {
				return false
			}
		}
		return true
	})
	if m := most.Load(); m != 1 {
		t.Fatalf("%d re-reads of B at once, want 1", m)
	}
	if c, d := calls.Load(), drains.Load(); c != d {
		t.Fatalf("%d batch refreshes for %d drained batches, want one each", c, d)
	}
	if p := pieces.Load(); int(p) != tb.NumPieces() {
		t.Fatalf("batch refreshes carried %d pieces, want the %d of the burst", p, tb.NumPieces())
	}
}

// A piece one Client of the pod marks not complete (a failed hash) is taken
// back from the other Client too: the bytes on disk are no longer verified.
func TestSiblings_NotCompleteReachesSibling(t *testing.T) {
	data, mi := addTestPayload(t)
	a, b := podClients(t)
	tb, err := addTorrent(b, mi)
	if err != nil {
		t.Fatal(err)
	}
	ta, err := addTorrent(a, mi)
	if err != nil {
		t.Fatal(err)
	}
	ta.DownloadAll()
	ta.AddClientPeer(seedingClient(t, data, mi))
	waitFor(t, "B to see piece 0 complete", func() bool { return tb.Piece(0).State().Complete })
	if err := ta.Piece(0).Storage().MarkNotComplete(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "B to see piece 0 not complete", func() bool { return !tb.Piece(0).State().Complete })
}

// shared_torrents counts an infohash once while two storages or more hold it,
// and a closed storage leaves the registry.
func TestSiblings_SharedTorrentsGauge(t *testing.T) {
	_, mi := addTestPayload(t)
	info, _ := mi.UnmarshalInfo()
	m := NewMMap(t.TempDir(), 0, FileCacheConfig{})
	m.siblings = newStorageSiblings()
	g0 := testutil.ToFloat64(promSharedTorrents)
	var open []storage.TorrentImpl
	for i, want := range []float64{0, 1, 1, 1, 0, 0} {
		if i < 3 {
			ts, err := m.OpenTorrent(context.Background(), &info, mi.HashInfoBytes())
			if err != nil {
				t.Fatal(err)
			}
			open = append(open, ts)
		} else {
			if err := open[i-3].Close(); err != nil {
				t.Fatal(err)
			}
		}
		if d := testutil.ToFloat64(promSharedTorrents) - g0; d != want {
			t.Fatalf("step %d: shared_torrents +%v, want +%v", i, d, want)
		}
	}
}
