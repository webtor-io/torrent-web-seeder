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
)

func init() {
	prometheus.MustRegister(promActiveTorrentCount)
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
	if s.v != nil {
		wsURL, err := s.v.GetWebseedURL(ctx, h)
		if err != nil {
			log.WithError(err).Errorf("failed to get webseed url for %s", h)
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
