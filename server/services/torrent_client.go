package services

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"

	"code.cloudfoundry.org/bytefmt"
	tlog "github.com/anacrolix/log"
	"github.com/anacrolix/torrent"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/mse"
	"github.com/anacrolix/torrent/storage"
	"golang.org/x/time/rate"
)

type TorrentClient struct {
	cl            *torrent.Client
	mux           sync.Mutex
	err           error
	inited        bool
	rLimit        int64
	dataDir       string
	completionDir string
	proxy         string
	port          int
}

const (
	TORRENT_CLIENT_DOWNLOAD_RATE_FLAG = "download-rate"
	HTTP_PROXY_FLAG                   = "http-proxy"
	COMPLETION_DIR_FLAG               = "completion-dir"
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
			Name:   HTTP_PROXY_FLAG,
			Usage:  "http proxy",
			Value:  "",
			EnvVar: "HTTP_PROXY",
		},
		cli.StringFlag{
			Name:   DATA_DIR_FLAG,
			Usage:  "data dir",
			Value:  os.TempDir(),
			EnvVar: "DATA_DIR",
		},
		cli.StringFlag{
			Name:   COMPLETION_DIR_FLAG,
			Usage:  "completion dir",
			Value:  os.TempDir(),
			EnvVar: "COMPLETION_DIR",
		},
	)
}

func NewTorrentClient(c *cli.Context, port int) (*TorrentClient, error) {
	dr := int64(-1)
	if c.String(TORRENT_CLIENT_DOWNLOAD_RATE_FLAG) != "" {
		drp, err := bytefmt.ToBytes(c.String(TORRENT_CLIENT_DOWNLOAD_RATE_FLAG))
		if err != nil {
			return nil, errors.Wrap(err, "failed to parse download rate flag")

		}
		dr = int64(drp)
	}
	return &TorrentClient{
		rLimit:        dr,
		dataDir:       c.String(DATA_DIR_FLAG),
		completionDir: c.String(COMPLETION_DIR_FLAG),
		proxy:         c.String(HTTP_PROXY_FLAG),
		port:          port,
	}, nil
}

func (s *TorrentClient) get() (*torrent.Client, error) {
	log.Infof("initializing TorrentClient dataDir=%v", s.dataDir)
	cfg := torrent.NewDefaultClientConfig()
	cfg.NoUpload = true
	// cfg.DisableAggressiveUpload = true
	cfg.Seed = false
	// cfg.AcceptPeerConnections = false
	// cfg.DisableIPv6 = true
	cfg.Logger = tlog.Default.WithNames("main", "client")
	// cfg.Debug = true
	cfg.DefaultStorage = storage.NewFileWithCustomPathMakerAndCompletion(
		s.dataDir,
		infoHashPathMaker,
		pieceCompletionForDir(s.completionDir),
	)
	cfg.ListenPort = s.port
	// cfg.DisableTrackers = true
	// cfg.DisableWebtorrent = true
	// cfg.DisableWebseeds = true
	// cfg.HeaderObfuscationPolicy = torrent.HeaderObfuscationPolicy{
	// 	Preferred:        true,
	// 	RequirePreferred: true,
	// }
	// cfg.CryptoSelector = MyCryptoSelector
	// cfg.PeriodicallyAnnounceTorrentsToDht = false
	if s.proxy != "" {
		url, err := url.Parse(s.proxy)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to parse proxy url=%v", s.proxy)
		}
		cfg.HTTPProxy = http.ProxyURL(url)
	}
	// cfg.Logger = torrentlogger.Discard
	// cfg.DefaultRequestStrategy = torrent.RequestStrategyFuzzing()
	// cfg.EstablishedConnsPerTorrent = 100
	// cfg.HalfOpenConnsPerTorrent = 50
	// cfg.TorrentPeersHighWater = 1000
	// cfg.TorrentPeersLowWater = 500
	cfg.TotalHalfOpenConns = 1000
	if s.rLimit != -1 {
		cfg.DownloadRateLimiter = rate.NewLimiter(rate.Limit(s.rLimit), int(s.rLimit))
	}
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create new torrent client")
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

func MyCryptoSelector(provided mse.CryptoMethod) mse.CryptoMethod {
	return mse.CryptoMethodRC4
}

func infoHashPathMaker(baseDir string, info *metainfo.Info, infoHash metainfo.Hash) string {
	return filepath.Join(baseDir, infoHash.HexString())
}

func pieceCompletionForDir(dir string) (ret storage.PieceCompletion) {
	ret, err := storage.NewDefaultPieceCompletionForDir(dir)
	if err != nil {
		log.Printf("couldn't open piece completion db in %q: %s", dir, err)
		ret = storage.NewMapPieceCompletion()
	}
	return
}
