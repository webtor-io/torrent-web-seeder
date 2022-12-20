package services

import (
	"github.com/prometheus/client_golang/prometheus"
	"sort"
	"sync"
	"time"

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
	tc  *TorrentClient
	tsm *TorrentStoreMap
	fsm *FileStoreMap
	tm  *TouchMap
	//magnet string
	timers map[string]*time.Timer
	ttl    time.Duration
	mux    sync.Mutex
}

func NewTorrentMap(tc *TorrentClient, tsm *TorrentStoreMap, fsm *FileStoreMap, tm *TouchMap) *TorrentMap {
	return &TorrentMap{
		tc:     tc,
		tsm:    tsm,
		fsm:    fsm,
		tm:     tm,
		timers: map[string]*time.Timer{},
		//magnet: c.String(MagnetFlag),
		ttl: time.Duration(600) * time.Second,
	}
}

func (s *TorrentMap) Get(h string) (*torrent.Torrent, error) {
	s.mux.Lock()
	defer s.mux.Unlock()
	cl, err := s.tc.Get()
	if err != nil {
		return nil, err
	}
	var t *torrent.Torrent
	// if s.magnet != "" {
	// 	sp, err := torrent.TorrentSpecFromMagnetUri(s.magnet)
	// 	if err != nil {
	// 		return nil, errors.Wrap(err, "failed to parse magnet")
	// 	}
	// 	if h == sp.InfoHash.HexString() {
	// 		t, err = cl.AddMagnet(s.magnet)
	// 		if err != nil {
	// 			return nil, errors.Wrap(err, "failed to add magnet")
	// 		}
	// 	}
	// }
	if t == nil {
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
		if mi != nil {
			t, err = cl.AddTorrent(mi)
			if err != nil {
				return nil, err
			}
			promActiveTorrentCount.Inc()
			ti, ok := s.timers[h]
			if ok {
				ti.Reset(s.ttl)
			} else {
				log.Infof("torrent added infohash=%v", h)
				promActiveTorrentCount.Inc()
				s.timers[h] = time.NewTimer(s.ttl)
				go func(h string) {
					<-s.timers[h].C
					s.mux.Lock()
					defer s.mux.Unlock()
					delete(s.timers, h)
					log.Infof("torrent dropped infohash=%v", h)
					t.Drop()
					promActiveTorrentCount.Dec()
				}(h)
			}
			_ = s.tm.Touch(h)
		}
	}
	return t, nil
}

func (s *TorrentMap) List() ([]string, error) {
	// cl, err := s.tc.Get()
	// if err != nil {
	// 	return nil, err
	// }
	// if s.magnet != "" {
	// 	sp, err := torrent.TorrentSpecFromMagnetUri(s.magnet)
	// 	if err != nil {
	// 		return nil, errors.Wrap(err, "failed to parse magnet")
	// 	}
	// 	r[sp.InfoHash.HexString()] = true
	// }
	// for _, t := range cl.Torrents() {
	// 	r[t.InfoHash().HexString()] = true
	// }
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
