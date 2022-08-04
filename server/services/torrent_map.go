package services

import (
	"sort"

	"github.com/anacrolix/torrent"
	"github.com/pkg/errors"
	"github.com/urfave/cli"
)

const (
	MAGNET = "magnet"
)

func RegisterTorrentMapFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.StringFlag{
			Name:   MAGNET,
			Usage:  "magnet",
			EnvVar: "MAGNET",
		},
	)
}

type TorrentMap struct {
	tc     *TorrentClient
	tsm    *TorrentStoreMap
	fsm    *FileStoreMap
	tm     *TouchMap
	magnet string
	c      *cli.Context
}

func NewTorrentMap(c *cli.Context, tc *TorrentClient, tsm *TorrentStoreMap, fsm *FileStoreMap, tm *TouchMap) *TorrentMap {
	return &TorrentMap{
		tc:     tc,
		tsm:    tsm,
		fsm:    fsm,
		tm:     tm,
		c:      c,
		magnet: c.String(MAGNET),
	}
}

func (s *TorrentMap) Get(h string) (*torrent.Torrent, error) {
	// cl, err := s.tc.Get()
	tc, err := NewTorrentClient(s.c)
	if err != nil {
		return nil, err
	}
	cl, err := tc.Get()
	if err != nil {
		return nil, err
	}
	var t *torrent.Torrent
	if s.magnet != "" {
		sp, err := torrent.TorrentSpecFromMagnetUri(s.magnet)
		if err != nil {
			return nil, errors.Wrap(err, "failed to parse magnet")
		}
		if h == sp.InfoHash.HexString() {
			t, err = cl.AddMagnet(s.magnet)
			if err != nil {
				return nil, errors.Wrap(err, "failed to add magnet")
			}
		}
	}
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
		}
	}
	s.tm.Touch(h)
	return t, nil
}

func (s *TorrentMap) List() ([]string, error) {
	cl, err := s.tc.Get()
	if err != nil {
		return nil, err
	}
	r := map[string]bool{}
	if s.magnet != "" {
		sp, err := torrent.TorrentSpecFromMagnetUri(s.magnet)
		if err != nil {
			return nil, errors.Wrap(err, "failed to parse magnet")
		}
		r[sp.InfoHash.HexString()] = true
	}
	for _, t := range cl.Torrents() {
		r[t.InfoHash().HexString()] = true
	}
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
