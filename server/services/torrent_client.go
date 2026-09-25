package services

import (
	"context"
	"log/slog"
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
	prometheus.MustRegister(promHandshakeSuccess)
	prometheus.MustRegister(promEstablishedConns)
	prometheus.MustRegister(promHalfOpenConns)
	prometheus.MustRegister(promSwarm)
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

// newPeerClient creates a client whose peer dialers are its own listen
// sockets, TCP and uTP, each wrapped in a metricsDialer so that every real
// dial is counted. It overrides cfg.DialForPeerConns, which would register
// the same sockets unwrapped.
//
// The dials used to be counted by two dialers added next to the sockets: a
// plain "tcp" and a plain "udp" net.Dialer. The library races all dialers
// and runs the handshake on the first connection returned. A UDP dial sends
// nothing and returns at once, so the bare UDP socket won nearly every race.
// No peer speaks BitTorrent over bare UDP (over UDP it is uTP), so the
// handshake waited out HANDSHAKE_TIMEOUT before the library turned to the
// TCP or uTP connection that had been ready all along, retrying it without
// the preferred header obfuscation. 66% of the prod first peers landed in the
// 2.9-4.3 s bucket at a 3 s timeout (2026-09-25, 17.3k torrents a day). In a
// local A/B with the prod flags on public torrents the first peer came at p50
// 3.2 s with the UDP dialer and 0.8 s without it, 24 runs each. The extra
// "tcp" dialer opened a second connection to every peer.
func newPeerClient(cfg *torrent.ClientConfig) (*torrent.Client, error) {
	cfg.DialForPeerConns = false
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	for _, l := range cl.Listeners() {
		if d, ok := l.(torrent.Dialer); ok {
			cl.AddDialer(&metricsDialer{network: d.DialerNetwork(), dialer: d})
		}
	}
	return cl, nil
}

type TorrentClient struct {
	cl                         *torrent.Client
	swarm                      *swarmStats                 // exports the clients' peer traffic
	testConfig                 func(*torrent.ClientConfig) // tests point the client at loopback; nil in the service
	cacheEvents                *CacheEvents                // set before the first Get; nil publishes nothing
	storageImpl                *mmapClientImpl
	mux                        sync.Mutex
	err                        error
	inited                     bool
	rLimit                     int64
	fileCache                  FileCacheConfig
	clientIdleTimeout          time.Duration
	dataDir                    string
	proxy                      string
	ua                         string
	noUpload                   bool
	seed                       bool
	dUTP                       bool
	dIPv6                      bool
	dWebTorrent                bool
	dWebseeds                  bool
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
	perTorrentCacheBudget      int64
	torrentClientDebug         bool
}

const (
	TorrentClientDownloadRateFlag  = "download-rate"
	TorrentClientUserAgentFlag     = "user-agent"
	HttpProxyFlag                  = "http-proxy"
	NoUploadFlag                   = "no-upload"
	SeedFlag                       = "seed"
	DisableUtpFlag                 = "disable-utp"
	DisableIPv6Flag                = "disable-ipv6"
	DisableWebTorrentFlag          = "disable-webtorrent"
	DisableWebseedsFlag            = "disable-webseeds"
	EstablishedConnsPerTorrentFlag = "established-conns-per-torrent"
	HalfOpenConnsPerTorrentFlag    = "half-open-conns-per-torrent"
	TorrentPeersHighWaterFlag      = "torrent-peers-high-water"
	TorrentPeersLowWaterFlag       = "torrent-peers-low-water"
	Debug                          = "debug"
	TorrentClientDebugFlag         = "torrent-client-debug"
	MaxUnverifiedBytesFlag         = "max-unverified-bytes"
	TotalHalfOpenConnsFlag         = "total-half-open-conns"
	MinDialTimeoutFlag             = "min-dial-timeout"
	NominalDialTimeoutFlag         = "nominal-dial-timeout"
	HandshakeTimeoutFlag           = "handshake-timeout"
	KeepAliveTimeoutFlag           = "keep-alive-timeout"
	PieceHashersPerTorrentFlag     = "piece-hashers-per-torrent"
	DialRateLimitFlag              = "dial-rate-limit"
	PerTorrentCacheBudgetFlag      = "per-torrent-cache-budget"
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
			Name:   DisableWebseedsFlag,
			Usage:  "disables webseeds",
			EnvVar: "DISABLE_WEBSEEDS",
		},
		cli.BoolFlag{
			Name:   DisableUtpFlag,
			Usage:  "disables utp",
			EnvVar: "DISABLE_UTP",
		},
		cli.BoolFlag{
			Name:   DisableIPv6Flag,
			Usage:  "disables IPv6: no IPv6 listening, peers or tracker announces (for networks without an IPv6 route)",
			EnvVar: "DISABLE_IPV6",
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
			Usage:  "debug logging for the application",
			EnvVar: "DEBUG",
		},
		cli.BoolFlag{
			Name:   TorrentClientDebugFlag,
			Usage:  "verbose debug logging for anacrolix torrent client",
			EnvVar: "TORRENT_CLIENT_DEBUG",
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
			EnvVar: "KEEPALIVE_TIMEOUT",
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
		cli.StringFlag{
			Name:   PerTorrentCacheBudgetFlag,
			Usage:  "per-torrent cache budget (e.g. 50GB, 0 = unlimited)",
			Value:  "50GB",
			EnvVar: "PER_TORRENT_CACHE_BUDGET",
		},
		cli.IntFlag{
			Name:   "max-open-files-per-torrent",
			Usage:  "files a torrent keeps open at once (descriptors and mappings); the rest open on demand",
			Value:  2048,
			EnvVar: "MAX_OPEN_FILES_PER_TORRENT",
		},
		cli.Int64Flag{
			Name:   "mmap-min-file-size",
			Usage:  "files shorter than this are read with pread instead of a mapping, in bytes",
			Value:  64 * 1024,
			EnvVar: "MMAP_MIN_FILE_SIZE",
		},
		cli.Int64Flag{
			Name:   "mmap-max-file-size",
			Usage:  "files longer than this are read with pread instead of a mapping (page tables of a mapping are pod memory), in bytes",
			Value:  4 << 30,
			EnvVar: "MMAP_MAX_FILE_SIZE",
		},
		cli.Int64Flag{
			Name:   "mmap-budget-per-torrent",
			Usage:  "most bytes a torrent keeps mapped at once; files opened past it are read with pread (page tables of mappings are pod memory)",
			Value:  8 << 30,
			EnvVar: "MMAP_BUDGET_PER_TORRENT",
		},
		cli.DurationFlag{
			Name:   "client-idle-timeout",
			Usage:  "close the torrent client after this long without torrents (0 = never); a closed client restarts with an empty DHT",
			Value:  30 * time.Minute,
			EnvVar: "CLIENT_IDLE_TIMEOUT",
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
	var cacheBudget int64
	if c.String(PerTorrentCacheBudgetFlag) != "" && c.String(PerTorrentCacheBudgetFlag) != "0" {
		cb, err := bytefmt.ToBytes(c.String(PerTorrentCacheBudgetFlag))
		if err != nil {
			return nil, errors.Wrap(err, "failed to parse per-torrent cache budget flag")
		}
		cacheBudget = int64(cb)
	}
	return &TorrentClient{
		swarm:                      promSwarm,
		rLimit:                     dr,
		dataDir:                    c.String(DataDirFlag),
		proxy:                      c.String(HttpProxyFlag),
		ua:                         c.String(TorrentClientUserAgentFlag),
		dUTP:                       c.Bool(DisableUtpFlag),
		dIPv6:                      c.Bool(DisableIPv6Flag),
		dWebTorrent:                c.Bool(DisableWebTorrentFlag),
		dWebseeds:                  c.Bool(DisableWebseedsFlag),
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
		perTorrentCacheBudget:      cacheBudget,
		fileCache: FileCacheConfig{
			MaxOpen:    c.Int("max-open-files-per-torrent"),
			MmapMin:    c.Int64("mmap-min-file-size"),
			MmapMax:    c.Int64("mmap-max-file-size"),
			MmapBudget: c.Int64("mmap-budget-per-torrent"),
		},
		clientIdleTimeout:  c.Duration("client-idle-timeout"),
		torrentClientDebug: c.Bool(TorrentClientDebugFlag),
	}, nil
}

func (s *TorrentClient) get() (*torrent.Client, error) {
	log.Infof("initializing TorrentClient dataDir=%v", s.dataDir)
	cfg := torrent.NewDefaultClientConfig()
	if s.torrentClientDebug {
		cfg.Logger = tlog.Default.WithNames("main", "client")
		cfg.Debug = true
	} else {
		l := tlog.NewLogger()
		l.SetHandlers(tlog.DiscardHandler)
		cfg.Logger = l
		// The library's warnings and errors (storage read failures ahead of
		// the "0 N" reader panic, tracker/dial trouble) go to logrus; its
		// Info/Debug stay discarded with cfg.Logger. See anacrolix_log.go.
		cfg.Slogger = slog.New(newAnacrolixLogHandler(slog.LevelWarn))
	}
	s.storageImpl = NewMMap(s.dataDir, s.perTorrentCacheBudget, s.fileCache)
	s.storageImpl.events = s.cacheEvents
	cfg.DefaultStorage = s.storageImpl
	if s.ua != "" {
		cfg.HTTPUserAgent = s.ua
	}
	cfg.NoUpload = s.noUpload
	cfg.Seed = s.seed
	cfg.DisableUTP = s.dUTP
	// Without an IPv6 route every IPv6 tracker announce fails at once with
	// "sendto: network is unreachable" and is retried: 175-275k warnings an
	// hour across the prod seeders on 2026-09-23, whose pods have only a
	// link-local IPv6 address.
	cfg.DisableIPv6 = s.dIPv6
	cfg.DisableWebtorrent = s.dWebTorrent
	cfg.DisableWebseeds = s.dWebseeds
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
	if s.testConfig != nil {
		s.testConfig(cfg)
	}
	cl, err := newPeerClient(cfg)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create new torrent client")
	}
	s.swarm.attach(cl)
	// Wire client reference to storage for eviction VerifyData calls.
	s.storageImpl.SetClient(cl)
	log.Infof("TorrentClient started")
	// The client used to close 60 s after its last torrent was dropped. On
	// a pod that serves a torrent every few minutes that meant 485 closes a
	// day fleet-wide, each restart starting with an empty DHT: first peer
	// p50 3.8 s, p95 68 s (audit 2026-09-20). A client without torrents
	// costs ~100 MiB, so it now stays warm for clientIdleTimeout.
	ticker := time.NewTicker(60 * time.Second)
	metricsTicker := time.NewTicker(time.Second)
	go func() {
		defer metricsTicker.Stop()
		defer ticker.Stop()
		var idleSince time.Time
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
					idleSince = time.Time{}
					continue
				}
				if idleSince.IsZero() {
					idleSince = time.Now()
				}
				if s.clientIdleTimeout <= 0 || time.Since(idleSince) < s.clientIdleTimeout {
					continue
				}
				s.closeIdle(cl)
				return
			}
		}
	}()
	return cl, nil
}

// closeIdle closes cl if it is still the current client; the next Get builds a
// new one.
func (s *TorrentClient) closeIdle(cl *torrent.Client) {
	s.mux.Lock()
	if s.cl != cl {
		s.mux.Unlock()
		return
	}
	s.cl.Close()
	s.swarm.retire(cl)
	s.cl = nil
	s.inited = false
	s.mux.Unlock()
	log.Infof("closing TorrentClient")
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
	// Read s.cl once: an idle close can set it to nil meanwhile, and the
	// client retired has to be the one closed here.
	if cl := s.cl; cl != nil {
		log.Infof("closing TorrentClient")
		cl.Close()
		s.swarm.retire(cl)
	}
}

// SetCacheEvents makes the storage report completed and evicted files. It has
// to be called before the client is first built (Get): the storage is created
// there, and torrents opened before it would stay silent.
func (s *TorrentClient) SetCacheEvents(e *CacheEvents) {
	s.cacheEvents = e
}
