package services

import (
	"bytes"
	"context"
	"sync"
	"time"

	ts "bitbucket.org/vintikzzzz/torrent-store/torrent-store"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

type MetaInfo struct {
	cl       *TorrentStore
	infoHash string
	input    string
	mux      sync.Mutex
	mi       *metainfo.MetaInfo
	err      error
	inited   bool
}

const (
	META_INFO_INFO_HASH_FLAG = "info-hash"
	META_INFO_INPUT_FLAG     = "input"
)

func RegisterMetaInfoFlags(c *cli.App) {
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   META_INFO_INFO_HASH_FLAG,
		Usage:  "torrent infohash",
		EnvVar: "TORRENT_INFO_HASH, INFO_HASH",
	})
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:  META_INFO_INPUT_FLAG,
		Usage: "torrent file path",
	})
}

func NewMetaInfo(c *cli.Context, cl *TorrentStore) *MetaInfo {
	return &MetaInfo{cl: cl, infoHash: c.String(META_INFO_INFO_HASH_FLAG), input: c.String(META_INFO_INPUT_FLAG), inited: false}
}

func (s *MetaInfo) Get() (*metainfo.MetaInfo, error) {
	s.mux.Lock()
	defer s.mux.Unlock()
	if s.inited {
		return s.mi, s.err
	}
	s.mi, s.err = s.get()
	s.inited = true
	return s.mi, s.err
}

func (s *MetaInfo) get() (*metainfo.MetaInfo, error) {
	log.Info("Initializing MetaInfo")
	if s.input != "" {
		log.Info("Loading from file")
		return metainfo.LoadFromFile(s.input)
	}
	c, err := s.cl.Get()
	if err != nil {
		return nil, errors.Wrap(err, "Failed to get torrent store client")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	r, err := c.Pull(ctx, &ts.PullRequest{InfoHash: s.infoHash})
	if err != nil {
		return nil, errors.Wrap(err, "Failed to pull torrent from the torrent store")
	}
	reader := bytes.NewReader(r.Torrent)
	mi, err := metainfo.Load(reader)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to parse torrent")
	}
	log.Info("Torrent pulled successfully")
	return mi, nil
}
