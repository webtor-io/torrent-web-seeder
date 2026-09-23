package services

import (
	"context"
	"encoding/hex"
	"sort"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	log "github.com/sirupsen/logrus"
)

//const (
//	MagnetFlag = "magnet"
//)

//func RegisterTorrentMapFlags(f []cli.Flag) []cli.Flag {
//	return append(f,
//		cli.StringFlag{
//			Name:   MagnetFlag,
//			Usage:  "magnet",
//			EnvVar: "MagnetFlag",
//		},
//	)
//}

var (
	promActiveTorrentCount = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "torrent_web_seeder_active_torrents_count",
		Help: "Web Seeder active torrents count",
	})
	promTimeToFirstPeerMs = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "torrent_web_seeder_time_to_first_peer_ms",
		Help:    "Time to first peer in milliseconds",
		Buckets: prometheus.ExponentialBuckets(50, 1.5, 20),
	})
	promTimeTo10PeersMs = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "torrent_web_seeder_time_to_10_peers_ms",
		Help:    "Time to 10 peers in milliseconds",
		Buckets: prometheus.ExponentialBuckets(50, 1.5, 20),
	})
	promTimeTo30PeersMs = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "torrent_web_seeder_time_to_30_peers_ms",
		Help:    "Time to 30 peers in milliseconds",
		Buckets: prometheus.ExponentialBuckets(50, 1.5, 20),
	})
	promTimeToFirstByteMs = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "torrent_web_seeder_time_to_first_byte_ms",
		Help:    "Time to first byte in milliseconds",
		Buckets: prometheus.ExponentialBuckets(50, 1.5, 20),
	})
	promStallDiscoverySeconds = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "torrent_web_seeder_stall_discovery_seconds_total",
		Help: "Total number of seconds when there was no data transferred and no active peers",
	})
	promStallIdleSeconds = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "torrent_web_seeder_stall_idle_seconds_total",
		Help: "Total number of seconds when there was no data transferred and at least one active peer, but no data received yet",
	})
	promStallDownloadSeconds = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "torrent_web_seeder_stall_download_seconds_total",
		Help: "Total number of seconds when there was no data transferred and data was already received",
	})
)

func init() {
	prometheus.MustRegister(promActiveTorrentCount)
	prometheus.MustRegister(promTimeToFirstPeerMs)
	prometheus.MustRegister(promTimeTo10PeersMs)
	prometheus.MustRegister(promTimeTo30PeersMs)
	prometheus.MustRegister(promTimeToFirstByteMs)
	prometheus.MustRegister(promStallDiscoverySeconds)
	prometheus.MustRegister(promStallIdleSeconds)
	prometheus.MustRegister(promStallDownloadSeconds)
}

type TorrentMap struct {
	tc      *TorrentClient
	tsm     *TorrentStoreMap
	fsm     *FileStoreMap
	entries map[string]*torrentEntry
	ttl     time.Duration
	mux     sync.Mutex
}

// torrentEntry is a loaded torrent's lease: the TTL timer and the number of
// requests currently being served from it.
type torrentEntry struct {
	timer  *time.Timer
	active int
	drop   func()
}

func NewTorrentMap(tc *TorrentClient, tsm *TorrentStoreMap, fsm *FileStoreMap) *TorrentMap {
	return &TorrentMap{
		tc:      tc,
		tsm:     tsm,
		fsm:     fsm,
		entries: map[string]*torrentEntry{},
		ttl:     time.Duration(600) * time.Second,
	}
}

func (s *TorrentMap) Touch(h string) {
	s.mux.Lock()
	defer s.mux.Unlock()
	if e, ok := s.entries[h]; ok {
		e.timer.Reset(s.ttl)
	}
}

// Hold keeps torrent h loaded until the returned release is called, however
// long that takes. The TTL used to be refreshed only by bytes written to the
// response (TouchWriter), so a stream stalled on a dead swarm for ten
// minutes had its torrent dropped underneath the reader: with the eager
// span that ended the stream as a silent EOF, with lazySpan it is
// ErrSpanClosed, which anacrolix retries into a recovered panic
// (updatePieceCompletion "0 N", 81 a day after 749bb2c). A torrent nobody
// is reading still expires ttl after its last touch or release.
func (s *TorrentMap) Hold(h string) (release func()) {
	s.mux.Lock()
	defer s.mux.Unlock()
	e, ok := s.entries[h]
	if !ok {
		return func() {}
	}
	e.active++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mux.Lock()
			defer s.mux.Unlock()
			e.active--
			if e.active == 0 {
				e.timer.Reset(s.ttl)
			}
		})
	}
}

// track registers a freshly added torrent with a TTL and its drop action.
// Called with mux held.
func (s *TorrentMap) track(h string, drop func()) {
	e := &torrentEntry{drop: drop}
	e.timer = time.AfterFunc(s.ttl, func() { s.expire(h, e) })
	s.entries[h] = e
}

// expire runs when h's TTL fires: drops the torrent unless a request is
// being served from it, in which case the lease is extended by another ttl.
func (s *TorrentMap) expire(h string, e *torrentEntry) {
	s.mux.Lock()
	defer s.mux.Unlock()
	if cur, ok := s.entries[h]; !ok || cur != e {
		return
	}
	if e.active > 0 {
		log.Infof("torrent kept infohash=%v (%d active requests)", h, e.active)
		e.timer.Reset(s.ttl)
		return
	}
	delete(s.entries, h)
	log.Infof("torrent dropped infohash=%v", h)
	e.drop()
}

// Peek returns the torrent if this client already holds it — loaded by a
// stream, a download or a warm-up — without loading it and without touching
// its TTL. Stats look but do not touch: until 2026-09 every status request
// went through Get, so a page view joined the swarm on the viewer's behalf
// and kept the torrent alive, which is exactly what a headless farm hitting
// a handful of hashes was buying from us.
func (s *TorrentMap) Peek(h string) *torrent.Torrent {
	if len(h) != 40 {
		return nil
	}
	ih, err := hex.DecodeString(h)
	if err != nil {
		return nil
	}
	s.mux.Lock()
	defer s.mux.Unlock()
	cl, err := s.tc.Get()
	if err != nil {
		return nil
	}
	var mh metainfo.Hash
	copy(mh[:], ih)
	t, ok := cl.Torrent(mh)
	if !ok {
		return nil
	}
	return t
}

// MetaInfo returns the torrent's metainfo from the file store or the
// torrent store without loading the torrent into the client.
func (s *TorrentMap) MetaInfo(h string) (*metainfo.MetaInfo, error) {
	mi, err := s.fsm.Get(h)
	if err != nil {
		return nil, err
	}
	if mi == nil {
		mi, err = s.tsm.Get(h)
		if err != nil {
			return nil, err
		}
	}
	return mi, nil
}

// addTorrent adds mi to cl the way cl.AddTorrent does, minus the library's
// initial piece check. That check hashes every piece whose completion is not
// known, and pieceCompletion knows only the pieces it has a row for, so on
// add the library read and hashed every piece never downloaded: the whole
// torrent, from empty storage. On 2026-09-23 a 93k-piece torrent held a pod
// at its 5-core limit for 10 minutes, and when it was dropped the pieces
// still queued logged ~350k "storage span closed" warnings. A piece that does
// have a row needs no check: the row says whether it is complete. The price
// is that data on disk whose row was lost gets downloaded again.
func addTorrent(cl *torrent.Client, mi *metainfo.MetaInfo) (*torrent.Torrent, error) {
	spec, err := torrent.TorrentSpecFromMetaInfoErr(mi)
	if err != nil {
		return nil, err
	}
	spec.DisableInitialPieceCheck = true
	t, _, err := cl.AddTorrentSpec(spec)
	return t, err
}

func (s *TorrentMap) Get(ctx context.Context, h string) (*torrent.Torrent, error) {
	s.mux.Lock()
	defer s.mux.Unlock()
	cl, err := s.tc.Get()
	if err != nil {
		return nil, err
	}
	var t *torrent.Torrent
	mi, err := s.fsm.Get(h)
	if err != nil {
		return nil, err
	}
	if mi == nil {
		mi, err = s.tsm.Get(h)
		if err != nil {
			return nil, err
		}
	}
	if mi == nil {
		return nil, nil

	}
	t, err = addTorrent(cl, mi)
	if err != nil {
		return nil, err
	}
	e, ok := s.entries[h]
	if ok {
		e.timer.Reset(s.ttl)
	} else {
		log.Infof("torrent added infohash=%v", h)
		promActiveTorrentCount.Inc()
		startTime := time.Now()
		go func() {
			const tickDuration = time.Millisecond * 50
			ticker := time.NewTicker(tickDuration)
			defer ticker.Stop()
			firstPeerRecorded := false
			tenPeersRecorded := false
			thirtyPeersRecorded := false
			firstByteRecorded := false
			var lastBytesRead int64
			for {
				select {
				case <-t.Closed():
					return
				case <-ticker.C:
					stats := t.Stats()
					activePeers := stats.ActivePeers
					bytesRead := stats.ConnStats.BytesRead.Int64()
					if bytesRead == lastBytesRead {
						if activePeers == 0 {
							promStallDiscoverySeconds.Add(tickDuration.Seconds())
						} else if !firstByteRecorded {
							promStallIdleSeconds.Add(tickDuration.Seconds())
						} else {
							promStallDownloadSeconds.Add(tickDuration.Seconds())
						}
					}
					lastBytesRead = bytesRead
					if !firstPeerRecorded && activePeers > 0 {
						promTimeToFirstPeerMs.Observe(float64(time.Since(startTime).Milliseconds()))
						firstPeerRecorded = true
					}
					if !tenPeersRecorded && activePeers >= 10 {
						promTimeTo10PeersMs.Observe(float64(time.Since(startTime).Milliseconds()))
						tenPeersRecorded = true
					}
					if !thirtyPeersRecorded && activePeers >= 30 {
						promTimeTo30PeersMs.Observe(float64(time.Since(startTime).Milliseconds()))
						thirtyPeersRecorded = true
					}
					if !firstByteRecorded && stats.ConnStats.BytesReadUsefulData.Int64() > 0 {
						promTimeToFirstByteMs.Observe(float64(time.Since(startTime).Milliseconds()))
						firstByteRecorded = true
					}
				}
			}
		}()
		s.track(h, func() {
			t.Drop()
			promActiveTorrentCount.Dec()
		})
	}
	return t, nil
}

func (s *TorrentMap) List() ([]string, error) {
	r := map[string]bool{}
	l, err := s.fsm.List()
	if err != nil {
		return nil, err
	}
	for _, t := range l {
		r[t] = true
	}
	rr := []string{}
	for k := range r {
		rr = append(rr, k)
	}
	sort.Strings(rr)
	return rr, nil
}
