package services

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// zGauges reads cache_bytes_used and cache_pieces_count.
func zGauges() (float64, float64) {
	return testutil.ToFloat64(promCacheBytesUsed), testutil.ToFloat64(promCachePieceCount)
}

// The cache gauges are the sum of the open storages' LRUs, and each place that
// changed an LRU moved them by hand next to it. Where those two steps came
// apart the gauges drifted for good: a MarkComplete of a piece the LRU already
// had counted it again, and one that ran in Close after the gauges were taken
// down (the library's hashers go on after a drop) left its piece on them.
// A closed LRU is off the gauges, whatever still changes it.
func TestCacheGaugesFollowTheLRU(t *testing.T) {
	bytes0, count0 := zGauges()
	impl := zOpen(t, zSeed(t), nil)
	ts := zStorage(impl)
	check := func(when string) {
		t.Helper()
		ts.lru.mu.Lock()
		used, n := ts.lru.used, len(ts.lru.entries)
		ts.lru.mu.Unlock()
		b, c := zGauges()
		if b-bytes0 != float64(used) || c-count0 != float64(n) {
			t.Errorf("%s: the gauges are up %v bytes and %v pieces, the LRU holds %d and %d", when, b-bytes0, c-count0, used, n)
		}
	}
	check("open")
	zWritePiece(t, impl, zInfo(), 0)
	check("piece 0, which the LRU has, marked complete again")

	ts.pc = closeHookPC{ts.pc, func(closeDB func() error) error {
		zWritePiece(t, impl, zInfo(), 2) // a hash that finished in Close
		// and a Completion of a piece another pod marked incomplete
		if err := ts.pc.Set(metainfo.PieceKey{InfoHash: zIH, Index: 1}, false); err != nil {
			t.Error(err)
		}
		impl.Piece(zInfo().Piece(1)).Completion()
		return closeDB()
	}}
	if err := impl.Close(); err != nil {
		t.Fatal(err)
	}
	if b, c := zGauges(); b != bytes0 || c != count0 {
		t.Errorf("closed: the gauges are up %v bytes and %v pieces", b-bytes0, c-count0)
	}
}

// An eviction took its piece off cache_bytes_used as the LRU's use before the
// Remove less its use after: an Add in between (MarkComplete on another
// hasher) cancelled the piece out, "freed 0 bytes", and the gauge kept it.
// 2026-10-05: 60 such evictions on s5gp7 and 44 on p596l in a day.
func TestCacheGaugesUnderConcurrentEvictions(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test")
	}
	const (
		pieces   = 64
		pieceLen = 4 << 10
		hashers  = 8
	)
	info := &metainfo.Info{Name: "gauges", PieceLength: pieceLen, Length: pieces * pieceLen, Pieces: makeDummyPieces(pieces)}
	files, err := torrentSpanFiles(info, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	span := newLazySpan(files, FileCacheConfig{})
	bytes0, count0 := zGauges()
	ts := &mmapTorrentStorage{
		span:    span,
		pc:      storage.NewMapPieceCompletion(),
		lru:     NewPieceLRU(8 * pieceLen),
		info:    info,
		closeCh: make(chan struct{}),
		evicted: make([]atomic.Bool, pieces),
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	var wg sync.WaitGroup
	for h := 0; h < hashers; h++ {
		wg.Add(1)
		go func(h int) {
			defer wg.Done()
			for time.Now().Before(deadline) {
				for i := h; i < pieces; i += hashers {
					if ts.lru.Has(i) { // complete: the library does not hash it
						continue
					}
					ts.setEvicted(i, false) // as the piece's next WriteAt does
					_ = ts.Piece(info.Piece(i)).MarkComplete()
				}
			}
		}(h)
	}
	wg.Wait()
	ts.lru.mu.Lock()
	used, n := ts.lru.used, len(ts.lru.entries)
	ts.lru.mu.Unlock()
	b, c := zGauges()
	if b-bytes0 != float64(used) || c-count0 != float64(n) {
		t.Errorf("the gauges are up %v bytes and %v pieces, the LRU holds %d and %d", b-bytes0, c-count0, used, n)
	}
	_ = ts.Close()
}
