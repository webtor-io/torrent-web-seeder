package services

import (
	"slices"
	"sync"

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

// refreshFromStorage re-reads piece idx's completion in this storage's Client
// (Piece.RefreshCompletionFromStorage). No attached check: it runs in its own goroutine.
// With --torrent-client-debug the library may log "piece request order has {}
// and pending pieces has {N}" for a piece a reader waits on: it checks needData
// before it updates its pending set, a transient mismatch.
func (ts *mmapTorrentStorage) refreshFromStorage(idx int) {
	if ts.cl == nil {
		return
	}
	for _, t := range ts.cl.Torrents() {
		if t.InfoHash() == ts.infoHash {
			t.Piece(idx).RefreshCompletionFromStorage()
			return
		}
	}
}
