package services

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"code.cloudfoundry.org/bytefmt"
	tlog "github.com/anacrolix/log"
	"github.com/anacrolix/torrent"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"

	"golang.org/x/time/rate"
)

var (
	promDialAttempts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "torrent_web_seeder_dial_attempts_total",
		Help: "Total number of dial attempts",
	}, []string{"type"})
	promDialFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "torrent_web_seeder_dial_failures_total",
		Help: "Total number of dial failures",
	}, []string{"type", "err"})
	promDialSuccess = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "torrent_web_seeder_dial_success_total",
		Help: "Total number of dial successes",
	}, []string{"type"})
	promTimeToFirstPeerMs = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "torrent_web_seeder_time_to_first_peer_ms",
		Help:    "Time to first peer in milliseconds",
		Buckets: prometheus.ExponentialBuckets(100, 2, 10),
	})
	promHandshakeSuccess = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "torrent_web_seeder_handshake_success_total",
		Help: "Total number of successful handshakes",
	})
	promEstablishedConns = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "torrent_web_seeder_established_connections",
		Help: "Total number of established connections",
	})
	promHalfOpenConns = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "torrent_web_seeder_half_open_connections",
		Help: "Total number of half-open connections",
	})
)

func init() {
	prometheus.MustRegister(promDialAttempts)
	prometheus.MustRegister(promDialFailures)
	prometheus.MustRegister(promDialSuccess)
	prometheus.MustRegister(promTimeToFirstPeerMs)
	prometheus.MustRegister(promHandshakeSuccess)
	prometheus.MustRegister(promEstablishedConns)
	prometheus.MustRegister(promHalfOpenConns)
}

type metricsDialer struct {
	network string
	dialer  torrent.Dialer
}

func (m *metricsDialer) Dial(ctx context.Context, addr string) (net.Conn, error) {
	promDialAttempts.WithLabelValues("peer").Inc()
	conn, err := m.dialer.Dial(ctx, addr)
	if err != nil {
		promDialFailures.WithLabelValues("peer", categorizeError(err)).Inc()
	} else {
		promDialSuccess.WithLabelValues("peer").Inc()
	}
	return conn, err
}

func categorizeError(err error) string {
	if err == nil {
		return "none"
	}
	if os.IsTimeout(err) || errors.Is(err, context.DeadlineExceeded) || (errors.Unwrap(err) != nil && errors.Is(errors.Unwrap(err), context.DeadlineExceeded)) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	errStr := err.Error()
	if strings.Contains(errStr, "connection refused") {
		return "connection_refused"
	}
	if strings.Contains(errStr, "connection reset by peer") {
		return "connection_reset"
	}
	if strings.Contains(errStr, "no such host") {
		return "no_such_host"
	}
	return "other"
}

func (m *metricsDialer) DialerNetwork() string {
	return m.network
}

type TorrentClient struct {
	cl                         *torrent.Client
	mux                        sync.Mutex
	err                        error
	inited                     bool
	rLimit                     int64
	dataDir                    string
	proxy                      string
	ua                         string
	noUpload                   bool
	seed                       bool
	dUTP                       bool
	dWebTorrent                bool
	establishedConnsPerTorrent int
	halfOpenConnsPerTorrent    int
	torrentPeersHighWater      int
	torrentPeersLowWater       int
	debug                      bool
	maxUnverifiedBytes         int64
	totalHalfOpenConns         int
	minDialTimeout             time.Duration
	nominalDialTimeout         time.Duration
	handshakeTimeout           time.Duration
	keepAliveTimeout           time.Duration
	pieceHashersPerTorrent     int
	dialRateLimit              int
}

const (
	TorrentClientDownloadRateFlag  = "download-rate"
	TorrentClientUserAgentFlag     = "user-agent"
	HttpProxyFlag                  = "http-proxy"
	NoUploadFlag                   = "no-upload"
	SeedFlag                       = "seed"
	DisableUtpFlag                 = "disable-utp"
	DisableWebTorrentFlag          = "disable-webtorrent"
	EstablishedConnsPerTorrentFlag = "established-conns-per-torrent"
	HalfOpenConnsPerTorrentFlag    = "half-open-conns-per-torrent"
	TorrentPeersHighWaterFlag      = "torrent-peers-high-water"
	TorrentPeersLowWaterFlag       = "torrent-peers-low-water"
	Debug                          = "debug"
	MaxUnverifiedBytesFlag         = "max-unverified-bytes"
	TotalHalfOpenConnsFlag         = "total-half-open-conns"
	MinDialTimeoutFlag             = "min-dial-timeout"
	NominalDialTimeoutFlag         = "nominal-dial-timeout"
	HandshakeTimeoutFlag           = "handshake-timeout"
	KeepAliveTimeoutFlag           = "keep-alive-timeout"
	PieceHashersPerTorrentFlag     = "piece-hashers-per-torrent"
	DialRateLimitFlag              = "dial-rate-limit"
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
			Name:   NoUploadFlag,
			Usage:  "no upload",
			EnvVar: "NO_UPLOAD",
		},
		cli.BoolFlag{
			Name:   SeedFlag,
			Usage:  "seed",
			EnvVar: "SEED",
		},
		cli.BoolFlag{
			Name:   DisableWebTorrentFlag,
			Usage:  "disables WebTorrent",
			EnvVar: "DISABLE_WEBTORRENT",
		},
		cli.BoolFlag{
			Name:   DisableUtpFlag,
			Usage:  "disables utp",
			EnvVar: "DISABLE_UTP",
		},
		cli.IntFlag{
			Name:   EstablishedConnsPerTorrentFlag,
			Usage:  "established conns per torrent",
			EnvVar: "ESTABLISHED_CONNS_PER_TORRENT",
		},
		cli.IntFlag{
			Name:   HalfOpenConnsPerTorrentFlag,
			Usage:  "half-open conns per torrent",
			EnvVar: "HALF_OPEN_CONNS_PER_TORRENT",
		},
		cli.IntFlag{
			Name:   TorrentPeersHighWaterFlag,
			Usage:  "torrent peers high water",
			EnvVar: "TORRENT_PEERS_HIGH_WATER",
		},
		cli.IntFlag{
			Name:   TorrentPeersLowWaterFlag,
			Usage:  "torrent peers low water",
			EnvVar: "TORRENT_PEERS_LOW_WATER",
		},
		cli.BoolFlag{
			Name:   Debug,
			Usage:  "debug",
			EnvVar: "DEBUG",
		},
		cli.StringFlag{
			Name:   MaxUnverifiedBytesFlag,
			Usage:  "max unverified bytes",
			Value:  "",
			EnvVar: "MAX_UNVERIFIED_BYTES",
		},
		cli.IntFlag{
			Name:   TotalHalfOpenConnsFlag,
			Usage:  "total half open conns",
			EnvVar: "TOTAL_HALF_OPEN_CONNS",
		},
		cli.DurationFlag{
			Name:   MinDialTimeoutFlag,
			Usage:  "min dial timeout",
			EnvVar: "MIN_DIAL_TIMEOUT",
		},
		cli.DurationFlag{
			Name:   NominalDialTimeoutFlag,
			Usage:  "nominal dial timeout",
			EnvVar: "NOMINAL_DIAL_TIMEOUT",
		},
		cli.DurationFlag{
			Name:   HandshakeTimeoutFlag,
			Usage:  "handshake timeout",
			EnvVar: "HANDSHAKE_TIMEOUT",
		},
		cli.DurationFlag{
			Name:   KeepAliveTimeoutFlag,
			Usage:  "keep alive timeout",
			EnvVar: "KEEP_ALIVE_TIMEOUT",
		},
		cli.IntFlag{
			Name:   PieceHashersPerTorrentFlag,
			Usage:  "piece hashers per torrent",
			EnvVar: "PIECE_HASHERS_PER_TORRENT",
		},
		cli.IntFlag{
			Name:   DialRateLimitFlag,
			Usage:  "dial rate limit",
			EnvVar: "DIAL_RATE_LIMIT",
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
	ub := int64(-1)
	if c.String(MaxUnverifiedBytesFlag) != "" {
		uub, err := bytefmt.ToBytes(c.String(MaxUnverifiedBytesFlag))
		if err != nil {
			return nil, errors.Wrap(err, "failed to parse max unverified bytes flag")
		}
		ub = int64(uub)
	}
	return &TorrentClient{
		rLimit:                     dr,
		dataDir:                    c.String(DataDirFlag),
		proxy:                      c.String(HttpProxyFlag),
		ua:                         c.String(TorrentClientUserAgentFlag),
		dUTP:                       c.Bool(DisableUtpFlag),
		dWebTorrent:                c.Bool(DisableWebTorrentFlag),
		establishedConnsPerTorrent: c.Int(EstablishedConnsPerTorrentFlag),
		halfOpenConnsPerTorrent:    c.Int(HalfOpenConnsPerTorrentFlag),
		torrentPeersHighWater:      c.Int(TorrentPeersHighWaterFlag),
		torrentPeersLowWater:       c.Int(TorrentPeersLowWaterFlag),
		totalHalfOpenConns:         c.Int(TotalHalfOpenConnsFlag),
		noUpload:                   c.Bool(NoUploadFlag),
		seed:                       c.Bool(SeedFlag),
		maxUnverifiedBytes:         ub,
		debug:                      c.Bool(Debug),
		minDialTimeout:             c.Duration(MinDialTimeoutFlag),
		nominalDialTimeout:         c.Duration(NominalDialTimeoutFlag),
		handshakeTimeout:           c.Duration(HandshakeTimeoutFlag),
		keepAliveTimeout:           c.Duration(KeepAliveTimeoutFlag),
		pieceHashersPerTorrent:     c.Int(PieceHashersPerTorrentFlag),
		dialRateLimit:              c.Int(DialRateLimitFlag),
	}, nil
}

func (s *TorrentClient) get() (*torrent.Client, error) {
	log.Infof("initializing TorrentClient dataDir=%v", s.dataDir)
	cfg := torrent.NewDefaultClientConfig()
	// cfg.DisableIPv6 = true
	cfg.Logger = tlog.Default.WithNames("main", "client")
	cfg.Debug = s.debug
	cfg.DefaultStorage = NewMMap(s.dataDir)
	if s.ua != "" {
		cfg.HTTPUserAgent = s.ua
	}
	cfg.NoUpload = s.noUpload
	cfg.Seed = s.seed
	cfg.DisableUTP = s.dUTP
	cfg.DisableWebtorrent = s.dWebTorrent
	if s.proxy != "" {
		u, err := url.Parse(s.proxy)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to parse proxy u=%v", s.proxy)
		}
		cfg.HTTPProxy = http.ProxyURL(u)
	}
	if s.establishedConnsPerTorrent != 0 {
		cfg.EstablishedConnsPerTorrent = s.establishedConnsPerTorrent
	}
	if s.halfOpenConnsPerTorrent != 0 {
		cfg.HalfOpenConnsPerTorrent = s.halfOpenConnsPerTorrent
	}
	if s.torrentPeersHighWater != 0 {
		cfg.TorrentPeersHighWater = s.torrentPeersHighWater
	}
	if s.torrentPeersLowWater != 0 {
		cfg.TorrentPeersLowWater = s.torrentPeersLowWater
	}
	if s.totalHalfOpenConns != 0 {
		cfg.TotalHalfOpenConns = s.totalHalfOpenConns
	}
	if s.rLimit != -1 {
		cfg.DownloadRateLimiter = rate.NewLimiter(rate.Limit(s.rLimit), int(s.rLimit))
	}
	if s.maxUnverifiedBytes != -1 {
		cfg.MaxUnverifiedBytes = s.maxUnverifiedBytes
	}
	if s.dialRateLimit != 0 {
		cfg.DialRateLimiter = rate.NewLimiter(rate.Limit(s.dialRateLimit), s.dialRateLimit)
	}
	if s.nominalDialTimeout != 0 {
		cfg.NominalDialTimeout = s.nominalDialTimeout
	}
	if s.minDialTimeout != 0 {
		cfg.MinDialTimeout = s.minDialTimeout
	}
	if s.handshakeTimeout != 0 {
		cfg.HandshakesTimeout = s.handshakeTimeout
	}
	if s.keepAliveTimeout != 0 {
		cfg.KeepAliveTimeout = s.keepAliveTimeout
	}
	if s.pieceHashersPerTorrent != 0 {
		cfg.PieceHashersPerTorrent = s.pieceHashersPerTorrent
	}
	cfg.Callbacks.CompletedHandshake = func(pc *torrent.PeerConn, ih torrent.InfoHash) {
		promHandshakeSuccess.Inc()
	}
	cfg.HTTPDialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		promDialAttempts.WithLabelValues("http").Inc()
		d := net.Dialer{}
		conn, err := d.DialContext(ctx, network, addr)
		if err != nil {
			promDialFailures.WithLabelValues("http", categorizeError(err)).Inc()
		} else {
			promDialSuccess.WithLabelValues("http").Inc()
		}
		return conn, err
	}
	cfg.TrackerDialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		promDialAttempts.WithLabelValues("tracker").Inc()
		d := net.Dialer{}
		conn, err := d.DialContext(ctx, network, addr)
		if err != nil {
			promDialFailures.WithLabelValues("tracker", categorizeError(err)).Inc()
		} else {
			promDialSuccess.WithLabelValues("tracker").Inc()
		}
		return conn, err
	}
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create new torrent client")
	}
	cl.AddDialer(&metricsDialer{
		network: "tcp",
		dialer:  torrent.NetworkDialer{Network: "tcp", Dialer: *torrent.DefaultNetDialer},
	})
	cl.AddDialer(&metricsDialer{
		network: "udp",
		dialer:  torrent.NetworkDialer{Network: "udp", Dialer: *torrent.DefaultNetDialer},
	})
	log.Infof("TorrentClient started")
	ticker := time.NewTicker(60 * time.Second)
	metricsTicker := time.NewTicker(time.Second)
	go func() {
		defer metricsTicker.Stop()
		defer ticker.Stop()
		for {
			select {
			case <-cl.Closed():
				return
			case <-metricsTicker.C:
				stats := cl.Stats()
				promEstablishedConns.Set(float64(stats.ActivePeers))
				promHalfOpenConns.Set(float64(stats.ActiveHalfOpenAttempts))
			case <-ticker.C:
				if len(cl.Torrents()) != 0 {
					continue
				}
				s.mux.Lock()
				if s.cl != cl {
					s.mux.Unlock()
					return
				}
				s.cl.Close()
				s.cl = nil
				s.inited = false
				s.mux.Unlock()
				log.Infof("closing TorrentClient")
				return
			}
		}
	}()
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
