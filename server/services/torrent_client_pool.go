package services

import (
	"sync"
	"time"

	"github.com/urfave/cli"
)

const (
	TORRENT_CLIENT_POOL_SIZE_FLAG  = "torrent-client-pool-size"
	TORRENT_CLIENT_START_PORT_FLAG = "torrent-client-start-port"
	TORRENT_CLIENT_TTL_FLAG        = "torrent-client-ttl"
)

func RegisterTorrentClientPoolFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.IntFlag{
			Name:   TORRENT_CLIENT_POOL_SIZE_FLAG,
			Usage:  "torrent client pool size",
			Value:  100,
			EnvVar: "TORRENT_CLIENT_POOL_SIZE",
		},
		cli.IntFlag{
			Name:   TORRENT_CLIENT_START_PORT_FLAG,
			Usage:  "torrent-client-start-port",
			Value:  42069,
			EnvVar: "TORRENT_CLIENT_START_PORT",
		},
		cli.IntFlag{
			Name:   TORRENT_CLIENT_TTL_FLAG,
			Usage:  "torrent-client-ttl (sec)",
			Value:  3600,
			EnvVar: "TORRENT_CLIENT_TTL",
		},
	)
}

type TorrentClientPool struct {
	m      map[string]*TorrentClient
	timers map[string]*time.Timer
	ttl    time.Duration
	mux    sync.Mutex
	start  int
	c      *cli.Context
}

func NewTorrentClientPool(c *cli.Context) *TorrentClientPool {
	return &TorrentClientPool{
		start:  c.Int(TORRENT_CLIENT_START_PORT_FLAG),
		m:      map[string]*TorrentClient{},
		timers: map[string]*time.Timer{},
		ttl:    time.Duration(c.Int(TORRENT_CLIENT_TTL_FLAG)) * time.Second,
		c:      c,
	}
}

func (s *TorrentClientPool) Get(h string) (res *TorrentClient, err error) {
	s.mux.Lock()
	defer s.mux.Unlock()
	res, ok := s.m[h]
	if ok {
		s.timers[h].Reset(s.ttl)
		return
	}
	var port int
	for i := s.start; true; i++ {
		found := false
		for _, tc := range s.m {
			if tc.port == i {
				found = true
				break
			}
		}
		if !found {
			port = i
			break
		}
	}
	res, err = NewTorrentClient(s.c, port, h)
	if err != nil {
		return
	}
	s.m[h] = res
	s.timers[h] = time.NewTimer(s.ttl)
	go func() {
		<-s.timers[h].C
		s.mux.Lock()
		defer s.mux.Unlock()
		delete(s.timers, h)
		s.m[h].Close()
		delete(s.m, h)
	}()
	return
}
