package services

import (
	"os"
	"sync"

	"code.cloudfoundry.org/bytefmt"
	"github.com/anacrolix/torrent"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"

	"github.com/anacrolix/torrent/storage"
	"golang.org/x/time/rate"
)

type TorrentClient struct {
	cl     *torrent.Client
	mux    sync.Mutex
	err    error
	inited bool
	rLimit int64
}

const (
	TORRENT_CLIENT_DOWNLOAD_RATE_FLAG = "download-rate"
)

func RegisterTorrentClientFlags(c *cli.App) {
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   TORRENT_CLIENT_DOWNLOAD_RATE_FLAG,
		Usage:  "download rate",
		Value:  "",
		EnvVar: "DOWNLOAD_RATE",
	})
}

func NewTorrentClient(c *cli.Context) (*TorrentClient, error) {
	dr := int64(-1)
	if c.String(TORRENT_CLIENT_DOWNLOAD_RATE_FLAG) != "" {
		drp, err := bytefmt.ToBytes(c.String(TORRENT_CLIENT_DOWNLOAD_RATE_FLAG))
		if err != nil {
			return nil, errors.Wrap(err, "Failed to parse download rate flag")

		}
		dr = int64(drp)
	}
	return &TorrentClient{rLimit: dr, inited: false}, nil
}

func (s *TorrentClient) get() (*torrent.Client, error) {
	log.Info("Initializing TorrentClient")
	cfg := torrent.NewDefaultClientConfig()
	cfg.Seed = false
	cfg.NoUpload = true
	cfg.DefaultStorage = storage.NewFile(os.TempDir())
	if s.rLimit != -1 {
		cfg.DownloadRateLimiter = rate.NewLimiter(rate.Limit(s.rLimit), int(s.rLimit))
	}
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to create new torrent client")
	}
	return cl, nil
}

func (s *TorrentClient) Get() (*torrent.Client, error) {
	s.mux.Lock()
	defer s.mux.Unlock()
	if s.inited {
		return s.cl, s.err
	}
	s.cl, s.err = s.get()
	s.inited = true
	return s.cl, s.err
}

func (s *TorrentClient) Close() {
	if s.cl != nil {
		s.cl.Close()
	}
}
