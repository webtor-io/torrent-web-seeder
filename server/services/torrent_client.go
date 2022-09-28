package services

import (
	"crypto/sha1"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"

	"code.cloudfoundry.org/bytefmt"
	tlog "github.com/anacrolix/log"
	"github.com/anacrolix/torrent"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"

	"github.com/anacrolix/torrent/mse"
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
	proxy   string
	port    int
	hash    string
	ua      string
}

const (
	TORRENT_CLIENT_DOWNLOAD_RATE_FLAG = "download-rate"
	TORRENT_CLIENT_USER_AGENT_FLAG    = "user-agent"
	HTTP_PROXY_FLAG                   = "http-proxy"
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
			Name:   TORRENT_CLIENT_USER_AGENT_FLAG,
			Usage:  "user agent",
			Value:  "",
			EnvVar: "USER_AGENT",
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
	)
}

func NewTorrentClient(c *cli.Context, port int, h string) (*TorrentClient, error) {
	dr := int64(-1)
	if c.String(TORRENT_CLIENT_DOWNLOAD_RATE_FLAG) != "" {
		drp, err := bytefmt.ToBytes(c.String(TORRENT_CLIENT_DOWNLOAD_RATE_FLAG))
		if err != nil {
			return nil, errors.Wrap(err, "failed to parse download rate flag")

		}
		dr = int64(drp)
	}
	return &TorrentClient{
		rLimit:  dr,
		dataDir: c.String(DATA_DIR_FLAG),
		proxy:   c.String(HTTP_PROXY_FLAG),
		port:    port,
		hash:    h,
		ua:      c.String(TORRENT_CLIENT_USER_AGENT_FLAG),
	}, nil
}

func (s *TorrentClient) distributeByHash(dirs []string, hash string) (string, error) {
	sort.Strings(dirs)
	hex := fmt.Sprintf("%x", sha1.Sum([]byte(hash)))[0:5]
	num64, err := strconv.ParseInt(hex, 16, 64)
	if err != nil {
		return "", errors.Wrapf(err, "failed to parse hex from hex=%v infohash=%v", hex, hash)
	}
	num := int(num64 * 1000)
	total := 1048575 * 1000
	interval := total / len(dirs)
	for i := 0; i < len(dirs); i++ {
		if num < (i+1)*interval {
			return dirs[i], nil
		}
	}
	return "", errors.Wrapf(err, "failed to distribute infohash=%v", hash)
}

func (s *TorrentClient) getDir() (string, error) {
	if strings.HasSuffix(s.dataDir, "*") {
		prefix := strings.TrimSuffix(s.dataDir, "*")
		dir, lp := path.Split(prefix)
		if dir == "" {
			dir = "."
		}
		files, err := ioutil.ReadDir(dir)
		if err != nil {
			return "", err
		}
		dirs := []string{}
		for _, f := range files {
			if f.IsDir() && strings.HasPrefix(f.Name(), lp) {
				dirs = append(dirs, f.Name())
			}
		}
		if len(dirs) == 0 {
			return prefix + "/" + s.hash, nil
		} else if len(dirs) == 1 {
			return dir + "/" + dirs[0] + "/" + s.hash, nil
		} else {
			d, err := s.distributeByHash(dirs, s.hash)
			if err != nil {
				return "", err
			}
			return dir + "/" + d + "/" + s.hash, nil
		}
	} else {
		return s.dataDir + "/" + s.hash, nil
	}
}

func (s *TorrentClient) get() (*torrent.Client, error) {
	d, err := s.getDir()
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get directory name")
	}
	_ = os.Mkdir(d, os.ModePerm)
	log.Infof("initializing TorrentClient dataDir=%v hash=%v port=%v", d, s.hash, s.port)
	cfg := torrent.NewDefaultClientConfig()
	cfg.NoUpload = true
	// cfg.DisableAggressiveUpload = true
	cfg.Seed = false
	// cfg.AcceptPeerConnections = false
	// cfg.DisableIPv6 = true
	cfg.Logger = tlog.Default.WithNames("main", "client")
	// cfg.Debug = true
	cfg.DefaultStorage = storage.NewMMap(d)
	cfg.ListenPort = s.port
	if s.ua != "" {
		cfg.HTTPUserAgent = s.ua
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
	// cfg.TotalHalfOpenConns = 1000
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
		log.Infof("closing TorrentClient hash=%v port=%v", s.hash, s.port)
		s.cl.Close()
	}
}

func MyCryptoSelector(provided mse.CryptoMethod) mse.CryptoMethod {
	return mse.CryptoMethodRC4
}
