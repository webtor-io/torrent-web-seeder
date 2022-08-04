package services

import (
	"sort"

	"github.com/anacrolix/torrent"
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
	tcp    *TorrentClientPool
	tsm    *TorrentStoreMap
	fsm    *FileStoreMap
	tm     *TouchMap
	magnet string
}

func NewTorrentMap(c *cli.Context, tcp *TorrentClientPool, tsm *TorrentStoreMap, fsm *FileStoreMap, tm *TouchMap) *TorrentMap {
	return &TorrentMap{
		tcp:    tcp,
		tsm:    tsm,
		fsm:    fsm,
		tm:     tm,
		magnet: c.String(MAGNET),
	}
}

func (s *TorrentMap) Get(h string) (*torrent.Torrent, error) {
	tc, err := s.tcp.Get(h)
	if err != nil {
		return nil, err
	}
	cl, err := tc.Get()
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
		}
	}
	s.tm.Touch(h)
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
