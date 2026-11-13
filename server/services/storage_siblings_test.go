package services

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"github.com/prometheus/client_golang/prometheus/testutil"
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

// siblingClient is a Client whose storage is rooted at dir and registered in
// sib, as TorrentClient.get builds it.
func siblingClient(t *testing.T, dir string, sib *storageSiblings) *torrent.Client {
	t.Helper()
	cfg := addTestConfig(dir)
	m := NewMMap(dir, 0, FileCacheConfig{})
	m.siblings = sib
	cfg.DefaultStorage = m
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m.SetClient(cl)
	t.Cleanup(func() { cl.Close() })
	return cl
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
			_, _ = a.Piece(p0).ReadAt(got, 0)
			if c.guarded != bytes.Equal(got, data[:1024]) {
				t.Fatalf("guarded=%v but the verified bytes kept=%v", c.guarded, bytes.Equal(got, data[:1024]))
			}
			if d := testutil.ToFloat64(promSharedChunkWritesDropped) - dropped; d != map[bool]float64{true: 1, false: 0}[c.guarded] {
				t.Fatalf("dropped +%v", d)
			}
			chunk := data[addTestPieceLen : addTestPieceLen+1024]
			if _, err := b.Piece(p1).WriteAt(chunk, 0); err != nil {
				t.Fatal(err)
			}
			_, _ = b.Piece(p1).ReadAt(got, 0)
			if !bytes.Equal(got, chunk) {
				t.Fatal("a piece nobody completed was not written")
			}
		})
	}
}

// B holds the torrent with no peer and a reader waiting; A downloads it. B's reader
// gets every byte from A's verified pieces and B downloads nothing.
func TestSiblings_ReaderGetsSiblingPiece(t *testing.T) {
	data, mi := addTestPayload(t)
	dir, sib := t.TempDir(), newStorageSiblings()
	a, b := siblingClient(t, dir, sib), siblingClient(t, dir, sib)
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
	s := tb.Stats()
	if n := s.ConnStats.BytesReadUsefulData.Int64(); n != 0 {
		t.Fatalf("B downloaded %d useful bytes, want 0", n)
	}
}

// A piece one Client of the pod marks not complete (a failed hash) is taken
// back from the other Client too: the bytes on disk are no longer verified.
func TestSiblings_NotCompleteReachesSibling(t *testing.T) {
	data, mi := addTestPayload(t)
	dir, sib := t.TempDir(), newStorageSiblings()
	a, b := siblingClient(t, dir, sib), siblingClient(t, dir, sib)
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
