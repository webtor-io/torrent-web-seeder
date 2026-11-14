package services

import (
	"maps"
	"slices"
	"sync"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	promSharedTorrents = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "torrent_web_seeder_shared_torrents",
		Help: "Torrents open in two or more Clients of the pod at once",
	})
	promSharedChunkWritesDropped = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "torrent_web_seeder_shared_chunk_writes_dropped_total",
		Help: "Chunk writes dropped because another Client of the pod had verified the piece",
	})
)

func init() {
	prometheus.MustRegister(promSharedTorrents, promSharedChunkWritesDropped)
}

// storageSiblings is the pod's registry of open storages by infohash: the Clients
// of one pod holding the same torrent on the shared data dir. A nil registry is
// "another pod" (tests): no refresh, no write guard.
//
// The storages already share the torrent's files and .torrent.db, as two pods
// on a node do. What they do not share is the library's piece bitmap: without
// a word from the Client that verified a piece, the other one downloads it
// again and writes over it under its readers.
type storageSiblings struct {
	mu sync.Mutex
	m  map[metainfo.Hash][]*mmapTorrentStorage
}

func newStorageSiblings() *storageSiblings {
	return &storageSiblings{m: make(map[metainfo.Hash][]*mmapTorrentStorage)}
}

// add registers ts under its infohash.
func (s *storageSiblings) add(ts *mmapTorrentStorage) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[ts.infoHash] = append(s.m[ts.infoHash], ts)
	if len(s.m[ts.infoHash]) == 2 {
		promSharedTorrents.Inc()
	}
}

// remove takes ts out; a storage not registered is left alone.
func (s *storageSiblings) remove(ts *mmapTorrentStorage) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.m[ts.infoHash]
	i := slices.Index(l, ts)
	if i < 0 {
		return
	}
	l = slices.Delete(l, i, i+1)
	if len(l) == 1 {
		promSharedTorrents.Dec()
	}
	if len(l) == 0 {
		delete(s.m, ts.infoHash)
	} else {
		s.m[ts.infoHash] = l
	}
}

// others returns a copy of the storages of ts's infohash other than ts.
func (s *storageSiblings) others(ts *mmapTorrentStorage) []*mmapTorrentStorage {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var ret []*mmapTorrentStorage
	for _, o := range s.m[ts.infoHash] {
		if o != ts {
			ret = append(ret, o)
		}
	}
	return ret
}

// shared reports whether two or more storages of the pod hold h.
func (s *storageSiblings) shared(h metainfo.Hash) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m[h]) > 1
}

// queueRefresh has this storage's Client re-read piece idx's completion,
// which a sibling has just changed. The pieces wait in one set, drained by
// one worker at a time: each re-read takes the Client's lock, and a goroutine
// per piece queued a writer per piece on it while a sibling downloaded,
// holding up the Client's own requests and handshakes behind them. A piece
// queued twice before the worker gets to it is re-read once.
func (ts *mmapTorrentStorage) queueRefresh(idx int) {
	ts.refreshMu.Lock()
	defer ts.refreshMu.Unlock()
	if ts.refreshPending != nil {
		ts.refreshPending[idx] = struct{}{}
		return
	}
	ts.refreshPending = map[int]struct{}{idx: {}}
	go ts.drainRefresh()
}

// drainRefresh re-reads the queued pieces, in index order, until none is
// left. refreshPending stays non-nil while it runs, so queueRefresh starts no
// second one.
func (ts *mmapTorrentStorage) drainRefresh() {
	for {
		ts.refreshMu.Lock()
		batch := ts.refreshPending
		if len(batch) == 0 {
			ts.refreshPending = nil
			ts.refreshMu.Unlock()
			return
		}
		ts.refreshPending = map[int]struct{}{}
		ts.refreshMu.Unlock()
		ts.refreshFromStorage(slices.Sorted(maps.Keys(batch))...)
	}
}

// refreshFromStorage re-reads the completion of pieces idx in this storage's
// Client in one call (Torrent.RefreshCompletionsFromStorage): one acquisition
// of the Client's lock and one new-conns pass for the whole batch. Piece by
// piece, each re-read took the lock and re-scanned the request order, so a
// Client fell thousands of pieces behind a sibling downloading a torrent of
// many pieces. No attached check: the worker never runs under OpenTorrent.
//
// ponytail: a drained batch is not split, and the Client's lock is held for
// all of it. A batch is whatever piled up while the worker waited, so after
// the Client's lock was held long elsewhere, the next batch is large. Chunk
// it if a reader of this Client must not wait that long.
func (ts *mmapTorrentStorage) refreshFromStorage(idx ...int) {
	if ts.refreshHook != nil {
		ts.refreshHook()
	}
	if ts.cl == nil {
		return
	}
	t, ok := ts.cl.Torrent(ts.infoHash)
	if !ok {
		return
	}
	refresh := (*torrent.Torrent).RefreshCompletionsFromStorage
	if ts.refreshPieces != nil {
		refresh = ts.refreshPieces
	}
	refresh(t, idx)
}
