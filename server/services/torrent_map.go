package services

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/anacrolix/torrent"
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
	tc     *TorrentClient
	tsm    *TorrentStoreMap
	fsm    *FileStoreMap
	timers map[string]*time.Timer
	ttl    time.Duration
	mux    sync.Mutex
	v      *Vault
}

func NewTorrentMap(tc *TorrentClient, tsm *TorrentStoreMap, fsm *FileStoreMap, vault *Vault) *TorrentMap {
	return &TorrentMap{
		tc:     tc,
		tsm:    tsm,
		fsm:    fsm,
		timers: map[string]*time.Timer{},
		ttl:    time.Duration(600) * time.Second,
		v:      vault,
	}
}

func (s *TorrentMap) Touch(h string) {
	s.mux.Lock()
	defer s.mux.Unlock()
	ti, ok := s.timers[h]
	if ok {
		ti.Reset(s.ttl)
	}
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
	t, err = cl.AddTorrent(mi)
	if err != nil {
		return nil, err
	}
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
			case <-ctx.Done():
				return
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
				if firstPeerRecorded && tenPeersRecorded && thirtyPeersRecorded && firstByteRecorded {
					// We continue the loop to keep tracking stall seconds even after all time-to-X metrics are recorded.
					// However, the original logic returned here.
					// If we return, we stop tracking stall seconds for this torrent.
					// But usually TorrentMap.Get is called when a torrent is requested.
					// If we want to track stalls for the lifetime of the torrent, we should probably not return.
				}
			}
		}
	}()
	if s.v != nil {
		wsURL, err := s.v.GetWebseedURL(ctx, h)
		if err != nil {
			log.WithError(err).Errorf("failed to get webseed url for %s", h)
		} else if wsURL == "" {
			log.Warnf("no webseed url for %s", h)
		} else {
			log.Infof("adding webseed %s for %s", wsURL, h)
			t.AddWebSeeds([]string{wsURL})
		}
	}
	ti, ok := s.timers[h]
	if ok {
		ti.Reset(s.ttl)
	} else {
		log.Infof("torrent added infohash=%v", h)
		promActiveTorrentCount.Inc()
		ti := time.NewTimer(s.ttl)
		s.timers[h] = ti
		go func(h string, ti *time.Timer) {
			<-ti.C
			s.mux.Lock()
			defer s.mux.Unlock()
			delete(s.timers, h)
			log.Infof("torrent dropped infohash=%v", h)
			t.Drop()
			promActiveTorrentCount.Dec()
		}(h, ti)
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
