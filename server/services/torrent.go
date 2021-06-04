package services

import (
	"sync"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"

	"github.com/anacrolix/torrent"
)

const (
	MAGNET = "magnet"
)

func RegisterTorrentFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.StringFlag{
			Name:   MAGNET,
			Usage:  "magnet",
			EnvVar: "MAGNET",
		},
	)
}

type Torrent struct {
	t      *torrent.Torrent
	tcS    *TorrentClient
	miS    *MetaInfo
	magnet string
	mux    sync.Mutex
	err    error
	inited bool
}

func NewTorrent(c *cli.Context, tcS *TorrentClient, miS *MetaInfo) *Torrent {
	return &Torrent{
		tcS:    tcS,
		miS:    miS,
		magnet: c.String(MAGNET),
	}
}

func (s *Torrent) Ready() bool {
	return s.t != nil
}

func (s *Torrent) get() (*torrent.Torrent, error) {
	log.Info("Initializing Torrent")
	cl, err := s.tcS.Get()
	if err != nil {
		return nil, err
	}
	var t *torrent.Torrent
	if s.magnet != "" {
		t, err = cl.AddMagnet(s.magnet)
		if err != nil {
			return nil, errors.Wrap(err, "Failed to add magnet")
		}
	} else {
		mi, err := s.miS.Get()
		if err != nil {
			return nil, err
		}
		t, err = cl.AddTorrent(mi)
		if err != nil {
			return nil, errors.Wrap(err, "Failed to add torrent")
		}
	}
	return t, nil
}

func (s *Torrent) Get() (*torrent.Torrent, error) {
	s.mux.Lock()
	defer s.mux.Unlock()
	if s.inited {
		return s.t, s.err
	}
	s.t, s.err = s.get()
	s.inited = true
	return s.t, s.err
}
