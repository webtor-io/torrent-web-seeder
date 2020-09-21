package services

import (
	"time"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

type FullDownload struct {
	t    *Torrent
	tc   *TorrentClient
	done chan struct{}
}

func NewFullDownload(c *cli.Context, t *Torrent, tc *TorrentClient) *FullDownload {
	return &FullDownload{t: t, tc: tc}
}

func (s *FullDownload) Start() error {
	t, err := s.t.Get()
	if err != nil {
		return errors.Wrapf(err, "Failed to get torrent")
	}
	tc, err := s.tc.Get()
	if err != nil {
		return errors.Wrapf(err, "Failed to get torrent client")
	}
	ticker := time.NewTicker(30 * time.Second)
	threshold := 0.5
	go func() {
		for range ticker.C {
			completed := 0
			for i := 0; i < t.NumPieces(); i++ {
				ps := t.PieceState(i)
				if ps.Complete {
					completed++
				}
			}
			if s.done == nil && float64(completed)/float64(t.NumPieces()) > threshold {
				log.Infof("Starting full download at %v%%", threshold*100)
				s.done = make(chan struct{})
				t.DownloadAll()
				tc.WaitAll()
				log.Info("Finish full download")
				close(s.done)
			}
		}
	}()
	return nil
}

func (s *FullDownload) Close() {
	if s.done == nil {
		return
	}
	select {
	case <-s.done:
	case <-time.After(30 * time.Minute):
	}
}
