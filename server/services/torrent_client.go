package services

import (
	"net/http"
	"net/url"
	"os"
	"sync"

	"code.cloudfoundry.org/bytefmt"
	tlog "github.com/anacrolix/log"
	"github.com/anacrolix/torrent"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"

	"golang.org/x/time/rate"
)

type TorrentClient struct {
	cl      *torrent.Client
	mux     sync.Mutex
	err     error
	inited  bool
	rLimit  int64
	dataDir string
	proxy   string
	ua      string
	dUTP    bool
}

const (
	TorrentClientDownloadRateFlag = "download-rate"
	TorrentClientUserAgentFlag    = "user-agent"
	HttpProxyFlag                 = "http-proxy"
	DisableUtpFlag                = "disable-utp"
)

func RegisterTorrentClientFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.StringFlag{
			Name:   TorrentClientDownloadRateFlag,
			Usage:  "download rate",
			Value:  "",
			EnvVar: "DOWNLOAD_RATE",
		},
		cli.StringFlag{
			Name:   TorrentClientUserAgentFlag,
			Usage:  "user agent",
			Value:  "",
			EnvVar: "USER_AGENT",
		},
		cli.StringFlag{
			Name:   HttpProxyFlag,
			Usage:  "http proxy",
			Value:  "",
			EnvVar: "HTTP_PROXY",
		},
		cli.StringFlag{
			Name:   DataDirFlag,
			Usage:  "data dir",
			Value:  os.TempDir(),
			EnvVar: "DATA_DIR",
		},
		cli.BoolFlag{
			Name:   DisableUtpFlag,
			Usage:  "disables utp",
			EnvVar: "DISABLE_UTP",
		},
	)
}

func NewTorrentClient(c *cli.Context) (*TorrentClient, error) {
	dr := int64(-1)
	if c.String(TorrentClientDownloadRateFlag) != "" {
		drp, err := bytefmt.ToBytes(c.String(TorrentClientDownloadRateFlag))
		if err != nil {
			return nil, errors.Wrap(err, "failed to parse download rate flag")

		}
		dr = int64(drp)
	}
	return &TorrentClient{
		rLimit:  dr,
		dataDir: c.String(DataDirFlag),
		proxy:   c.String(HttpProxyFlag),
		ua:      c.String(TorrentClientUserAgentFlag),
		dUTP:    c.Bool(DisableUtpFlag),
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
	cfg.DefaultStorage = NewMMap(s.dataDir)
	if s.ua != "" {
		cfg.HTTPUserAgent = s.ua
	}
	if s.dUTP {
		cfg.DisableUTP = true
	}
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
		u, err := url.Parse(s.proxy)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to parse proxy u=%v", s.proxy)
		}
		cfg.HTTPProxy = http.ProxyURL(u)
	}
	// cfg.Logger = torrentlogger.Discard
	// cfg.DefaultRequestStrategy = torrent.RequestStrategyFuzzing()
	cfg.EstablishedConnsPerTorrent = 100
	cfg.HalfOpenConnsPerTorrent = 50
	cfg.TorrentPeersHighWater = 1000
	cfg.TorrentPeersLowWater = 100
	cfg.TotalHalfOpenConns = 5000
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
		log.Infof("closing TorrentClient")
		s.cl.Close()
	}
}
