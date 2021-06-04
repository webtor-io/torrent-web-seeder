package services

import (
	"os"
	"sync"

	"code.cloudfoundry.org/bytefmt"
	"github.com/anacrolix/torrent"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"

	torrentlogger "github.com/anacrolix/log"
	"github.com/anacrolix/torrent/storage"
	"golang.org/x/time/rate"
)

type TorrentClient struct {
	cl      *torrent.Client
	mux     sync.Mutex
	err     error
	inited  bool
	rLimit  int64
	dataDir string
}

const (
	TORRENT_CLIENT_DOWNLOAD_RATE_FLAG = "download-rate"
	TORRENT_CLIENT_DATA_DIR_FLAG      = "data-dir"
)

func RegisterTorrentClientFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.StringFlag{
			Name:   TORRENT_CLIENT_DOWNLOAD_RATE_FLAG,
			Usage:  "download rate",
			Value:  "",
			EnvVar: "DOWNLOAD_RATE",
		},
		cli.StringFlag{
			Name:   TORRENT_CLIENT_DATA_DIR_FLAG,
			Usage:  "data dir",
			Value:  os.TempDir(),
			EnvVar: "DATA_DIR",
		},
	)
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
	return &TorrentClient{rLimit: dr, dataDir: c.String(TORRENT_CLIENT_DATA_DIR_FLAG), inited: false}, nil
}

func (s *TorrentClient) get() (*torrent.Client, error) {
	log.Infof("Initializing TorrentClient dataDir=%v", s.dataDir)
	cfg := torrent.NewDefaultClientConfig()
	cfg.Seed = false
	cfg.NoUpload = true
	cfg.DefaultStorage = storage.NewMMap(s.dataDir)
	cfg.Logger = torrentlogger.Discard
	// cfg.DefaultRequestStrategy = torrent.RequestStrategyFuzzing()
	cfg.EstablishedConnsPerTorrent = 100
	cfg.HalfOpenConnsPerTorrent = 50
	cfg.TorrentPeersHighWater = 1000
	cfg.TorrentPeersLowWater = 500
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
