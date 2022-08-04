package services

import (
	"bytes"
	"context"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/webtor-io/lazymap"
	ts "github.com/webtor-io/torrent-store/torrent-store"
)

type TorrentStoreMap struct {
	lazymap.LazyMap
	ts *TorrentStore
}

func NewTorrentStoreMap(ts *TorrentStore) *TorrentStoreMap {
	return &TorrentStoreMap{
		ts: ts,
		LazyMap: lazymap.New(&lazymap.Config{
			Capacity: 1000,
		}),
	}
}

func (s *TorrentStoreMap) get(h string) (*metainfo.MetaInfo, error) {
	c, err := s.ts.Get()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get torrent store client")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	r, err := c.Pull(ctx, &ts.PullRequest{InfoHash: h})
	if err != nil {
		return nil, errors.Wrap(err, "failed to pull torrent from the torrent store")
	}
	reader := bytes.NewReader(r.Torrent)
	mi, err := metainfo.Load(reader)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse torrent")
	}
	log.Info("torrent pulled successfully")
	return mi, nil
}

func (s *TorrentStoreMap) Get(h string) (*metainfo.MetaInfo, error) {
	mi, err := s.LazyMap.Get(h, func() (interface{}, error) {
		return s.get(h)
	})
	if err != nil {
		return nil, err
	}
	return mi.(*metainfo.MetaInfo), nil
}
