package services

import (
	"context"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	log "github.com/sirupsen/logrus"
)

//const (
//	MagnetFlag = "magnet"
//)

//func RegisterTorrentMapFlags(f []cli.Flag) []cli.Flag {
//	return append(f,
//		cli.StringFlag{
//			Name:   MagnetFlag,
//			Usage:  "magnet",
//			EnvVar: "MagnetFlag",
//		},
//	)
//}

var (
	promActiveTorrentCount = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "torrent_web_seeder_active_torrents_count",
		Help: "Web Seeder active torrents count",
	})
	promTimeToFirstPeerMs = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "torrent_web_seeder_time_to_first_peer_ms",
		Help:    "Time to first peer in milliseconds",
		Buckets: prometheus.ExponentialBuckets(50, 1.5, 20),
	})
	promTimeTo10PeersMs = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "torrent_web_seeder_time_to_10_peers_ms",
		Help:    "Time to 10 peers in milliseconds",
		Buckets: prometheus.ExponentialBuckets(50, 1.5, 20),
	})
	promTimeTo30PeersMs = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "torrent_web_seeder_time_to_30_peers_ms",
		Help:    "Time to 30 peers in milliseconds",
		Buckets: prometheus.ExponentialBuckets(50, 1.5, 20),
	})
	promTimeToFirstByteMs = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "torrent_web_seeder_time_to_first_byte_ms",
		Help:    "Time to first byte in milliseconds",
		Buckets: prometheus.ExponentialBuckets(50, 1.5, 20),
	})
	promStallDiscoverySeconds = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "torrent_web_seeder_stall_discovery_seconds_total",
		Help: "Total number of seconds when there was no data transferred and no active peers",
	})
	promStallIdleSeconds = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "torrent_web_seeder_stall_idle_seconds_total",
		Help: "Total number of seconds when there was no data transferred and at least one active peer, but no data received yet",
	})
	promStallDownloadSeconds = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "torrent_web_seeder_stall_download_seconds_total",
		Help: "Total number of seconds when there was no data transferred and data was already received",
	})
)

func init() {
	prometheus.MustRegister(promActiveTorrentCount)
	prometheus.MustRegister(promTimeToFirstPeerMs)
	prometheus.MustRegister(promTimeTo10PeersMs)
	prometheus.MustRegister(promTimeTo30PeersMs)
	prometheus.MustRegister(promTimeToFirstByteMs)
	prometheus.MustRegister(promStallDiscoverySeconds)
	prometheus.MustRegister(promStallIdleSeconds)
	prometheus.MustRegister(promStallDownloadSeconds)
}

// TorrentMap holds the loaded torrents by entryKey: h in the default Client,
// h#uid in uid's account Client.
type TorrentMap struct {
	tc       *TorrentClient
	tsm      *TorrentStoreMap
	fsm      *FileStoreMap
	records  *PrivateRecords // nil: the store keeps no private records (disabled)
	enforce  bool
	entries  map[string]*torrentEntry
	accounts map[string]*accountClient // by uid
	ttl      time.Duration
	mux      sync.Mutex
}

// torrentEntry is a loaded torrent's lease: the TTL timer and the number of
// requests currently being served from it.
type torrentEntry struct {
	timer  *time.Timer
	active int
	drop   func()
	// peers is kept by the torrent's watcher (Get); stats read it.
	peers *peerTimeline
	t     *torrent.Torrent
	uid   string     // "" in the default Client
	tiers [][]string // an account's: the record's trackers, as loaded
}

// peerTimeline is since when a loaded torrent has had connected peers, none
// of them a seeder, without a break: the clock of availability_known
// (availabilitySettle). The library does not say when a connection was made,
// and a clock started by the stat itself would restart whenever someone
// opens the page.
//
// A connected seeder stops the clock: it makes availability known by itself
// (computeAvailability), and the streak starts when the last one goes. The
// peers left have announced long ago, but a seeder is often gone only for a
// moment: chokeRedial drops a choking peer and dials it again
// (PeerConn.Redial: dropConnection, then addPeer), and until it is back the
// partial peers' union shows the reader's pieces as on nobody — "nobody has
// the piece you wait for" next to a seeder that is reconnecting.
//
// No peer at all breaks the streak only after peerGapGrace.
type peerTimeline struct {
	since atomic.Int64 // UnixNano; 0 while there is no streak
	// zeroSince is when the peer count fell to zero (UnixNano), 0 while
	// peers are connected. Only the watcher touches it.
	zeroSince int64
}

// peerGapGrace is how long a torrent may have no peer connected before its
// streak breaks. chokeRedial drops up to five connections in one pass; in a
// small swarm stuck on pieces nobody has, every peer chokes us and is
// redialled at once, the count sits at zero until a redial lands, and a
// break there would hide the holes for another 20 s every couple of
// minutes. Prod's dial (7 s nominal) and handshake (3 s) timeouts bound a
// redial. The cost: peers that come back after a gap are unread for their
// round trip, as a peer joining a running streak is. While nothing is
// connected computeAvailability does not settle anyway.
const peerGapGrace = 10 * time.Second

// observe records the active peer and connected seeder counts at now. One
// writer: the torrent's watcher, every 50 ms.
func (p *peerTimeline) observe(activePeers, seeders int, now time.Time) {
	switch {
	case seeders > 0:
		p.since.Store(0)
		p.zeroSince = 0
	case activePeers > 0:
		p.zeroSince = 0
		if p.since.Load() == 0 {
			p.since.Store(now.UnixNano())
		}
	case p.zeroSince == 0:
		p.zeroSince = now.UnixNano()
	case now.Sub(time.Unix(0, p.zeroSince)) >= peerGapGrace:
		p.since.Store(0)
	}
}

// connectedFor is how long the torrent has had peers and no seeder without a
// break at now (a gap shorter than peerGapGrace is not one): zero with a
// seeder, after a break, and for a torrent nobody tracks (nil).
func (p *peerTimeline) connectedFor(now time.Time) time.Duration {
	if p == nil {
		return 0
	}
	since := p.since.Load()
	if since == 0 {
		return 0
	}
	return now.Sub(time.Unix(0, since))
}

func NewTorrentMap(tc *TorrentClient, tsm *TorrentStoreMap, fsm *FileStoreMap) *TorrentMap {
	return &TorrentMap{
		tc:       tc,
		tsm:      tsm,
		fsm:      fsm,
		entries:  map[string]*torrentEntry{},
		accounts: map[string]*accountClient{},
		ttl:      time.Duration(600) * time.Second,
	}
}

// WithPrivate sets where private records come from and whether access to a
// private torrent is enforced or only observed (PRIVATE_ACCESS_ENFORCE).
func (s *TorrentMap) WithPrivate(pr *PrivateRecords, enforce bool) *TorrentMap {
	s.records, s.enforce = pr, enforce
	v := 0.0
	if enforce {
		v = 1
	}
	promPrivateEnforce.Set(v)
	return s
}

func (s *TorrentMap) Touch(h string) {
	s.mux.Lock()
	defer s.mux.Unlock()
	if e, ok := s.entries[h]; ok {
		e.timer.Reset(s.ttl)
	}
}

// Hold keeps torrent h loaded until the returned release is called, however
// long that takes. The TTL used to be refreshed only by bytes written to the
// response (TouchWriter), so a stream stalled on a dead swarm for ten
// minutes had its torrent dropped underneath the reader: with the eager
// span that ended the stream as a silent EOF, with lazySpan it is
// ErrSpanClosed, which anacrolix retries into a recovered panic
// (updatePieceCompletion "0 N", 81 a day after 749bb2c). A torrent nobody
// is reading still expires ttl after its last touch or release.
func (s *TorrentMap) Hold(h string) (release func()) {
	s.mux.Lock()
	defer s.mux.Unlock()
	e, ok := s.entries[h]
	if !ok {
		return func() {}
	}
	e.active++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mux.Lock()
			defer s.mux.Unlock()
			e.active--
			if e.active == 0 {
				e.timer.Reset(s.ttl)
			}
		})
	}
}

// reading reports whether a request is being served from torrent h.
func (s *TorrentMap) reading(h string) bool {
	s.mux.Lock()
	defer s.mux.Unlock()
	e, ok := s.entries[h]
	return ok && e.active > 0
}

// track registers a freshly added torrent with a TTL and its drop action.
// Called with mux held.
func (s *TorrentMap) track(key string, drop func()) *torrentEntry {
	e := &torrentEntry{drop: drop}
	e.timer = time.AfterFunc(s.ttl, func() { s.expire(key, e) })
	s.entries[key] = e
	return e
}

// expire runs when key's TTL fires: drops the torrent unless a request is
// being served from it, in which case the lease is extended by another ttl.
func (s *TorrentMap) expire(key string, e *torrentEntry) {
	s.mux.Lock()
	defer s.mux.Unlock()
	if cur, ok := s.entries[key]; !ok || cur != e {
		return
	}
	// The uid stays out of logs: the account goes as acct.
	h, _, _ := strings.Cut(key, "#")
	l := log.NewEntry(log.StandardLogger())
	if e.uid != "" {
		l = l.WithField("acct", acct(e.uid))
	}
	if e.active > 0 {
		l.Infof("torrent kept infohash=%v (%d active requests)", h, e.active)
		e.timer.Reset(s.ttl)
		return
	}
	s.unload(key, e)
	l.Infof("torrent dropped infohash=%v", h)
}

// unload takes key's torrent out of the map and drops it. Called with mux
// held.
func (s *TorrentMap) unload(key string, e *torrentEntry) {
	delete(s.entries, key)
	e.timer.Stop()
	e.drop()
}

// Peek returns the torrent if this client already holds it — loaded by a
// stream, a download or a warm-up — without loading it and without touching
// its TTL. Stats look but do not touch: until 2026-09 every status request
// went through Get, so a page view joined the swarm on the viewer's behalf
// and kept the torrent alive, which is exactly what a headless farm hitting
// a handful of hashes was buying from us.
//
// uid's own torrent (h#uid) comes first; without it, the default Client's.
func (s *TorrentMap) Peek(uid, h string) *torrent.Torrent {
	t, _ := s.peek(uid, h)
	return t
}

// peek is Peek with the torrent's peer timeline, read under the same lock;
// the timeline is nil for a torrent the map does not track.
func (s *TorrentMap) peek(uid, h string) (*torrent.Torrent, *peerTimeline) {
	if _, t, tl := s.statKey(uid, h); t != nil {
		return t, tl
	}
	if len(h) != 40 {
		return nil, nil
	}
	ih, err := hex.DecodeString(h)
	if err != nil {
		return nil, nil
	}
	s.mux.Lock()
	defer s.mux.Unlock()
	cl, err := s.tc.Get()
	if err != nil {
		return nil, nil
	}
	var mh metainfo.Hash
	copy(mh[:], ih)
	t, ok := cl.Torrent(mh)
	if !ok {
		return nil, nil
	}
	var tl *peerTimeline
	if e, ok := s.entries[h]; ok {
		tl = e.peers
	}
	return t, tl
}

// statKey is the stats' key for uid looking at h, resolved under one lock
// with the torrent it names: h#uid with uid's own torrent and timeline while
// that is loaded, so a frame kept under h#uid is always that torrent's;
// otherwise h and no torrent, h naming the default Client's, which
// peek("", h) finds when a frame is computed. Two accounts of one private
// torrent each see their own (tracker refusal, peers); everyone else shares
// h's frames.
func (s *TorrentMap) statKey(uid, h string) (string, *torrent.Torrent, *peerTimeline) {
	if uid == "" {
		return h, nil, nil
	}
	key := entryKey(h, uid)
	s.mux.Lock()
	defer s.mux.Unlock()
	if e, ok := s.entries[key]; ok {
		return key, e.t, e.peers
	}
	return h, nil, nil
}

// MetaInfo returns the torrent's metainfo from the file store or the
// torrent store without loading the torrent into the client.
func (s *TorrentMap) MetaInfo(h string) (*metainfo.MetaInfo, error) {
	mi, err := s.fsm.Get(h)
	if err != nil {
		return nil, err
	}
	if mi == nil {
		mi, err = s.tsm.Get(h)
		if err != nil {
			return nil, err
		}
	}
	return mi, nil
}

// addTorrent is how Get adds a torrent; the tests add through it too, so they
// exercise what production does. It is cl.AddTorrent, initial piece check
// included, and must stay so.
//
// The check hashes only pieces whose completion is unknown. pieceCompletion
// knows every piece (a missing row is "not complete", see its Get), so with
// it the check hashes nothing. What still answers unknown is the in-memory
// completion OpenTorrent falls back to when the db cannot be opened, and for
// that the check is the only way a piece becomes requestable: the library
// never requests an unknown piece. 8f0cf39 set DisableInitialPieceCheck here
// while Get still answered unknown for a missing row, and on 2026-09-24 no
// torrent new to a pod got a byte.
func addTorrent(cl *torrent.Client, mi *metainfo.MetaInfo) (*torrent.Torrent, error) {
	return cl.AddTorrent(mi)
}

// Get loads torrent h and returns it with its entryKey. A public torrent goes
// to the default Client under h, for any uid. A private one (BEP 27) goes to
// uid's account Client under h#uid, announcing only the trackers of uid's
// record in torrent-store; without a valid uid or a record enforce refuses it
// (seederRefusal) and observe loads it as before, into the default Client
// with the stored trackers. A malformed uid and a store that fails are
// refused in both modes.
//
// The metainfo and the record are read outside s.mux: a slow store holds up
// only the request that waits for it, not the pod's other torrents. A pair
// already loaded is served without asking the store again, so neither its
// outage nor the end of the record cache cuts a running stream; its
// refreshLoop re-reads the record and unloads it when the record is gone or
// its trackers changed.
func (s *TorrentMap) Get(ctx context.Context, h, uid string) (t *torrent.Torrent, key string, err error) {
	mi, err := s.MetaInfo(h)
	if err != nil || mi == nil {
		return nil, "", err
	}
	private, err := metainfoPrivate(mi)
	if err != nil {
		return nil, "", err
	}
	if !private {
		return s.getDefault(h, mi)
	}
	if uid == "" {
		return s.fallback(h, mi, reasonAccountRequired)
	}
	if !validUID(uid) {
		return nil, "", &seederRefusal{http.StatusForbidden, reasonAccountRequired}
	}
	key = entryKey(h, uid)
	if t := s.loaded(key); t != nil {
		return t, key, nil
	}
	rec, outcome := s.records.Get(h, uid)
	switch outcome {
	case recordDisabled:
		return s.fallback(h, mi, reasonPrivateDisabled)
	case recordBad:
		return s.fallback(h, mi, reasonBadRecord)
	case recordMissing:
		return s.fallback(h, mi, reasonNotEntitled)
	case recordUnavailable:
		return nil, "", &seederRefusal{http.StatusServiceUnavailable, reasonStoreUnavailable}
	}
	s.mux.Lock()
	defer s.mux.Unlock()
	// Another request of the pair may have loaded it during the lookup.
	if e, ok := s.entries[key]; ok {
		e.timer.Reset(s.ttl)
		return e.t, key, nil
	}
	a, err := s.accountFor(uid)
	if err != nil {
		return nil, "", err
	}
	m := *mi
	m.Announce = ""
	m.AnnounceList = rec.Tiers
	m.UrlList = nil
	t, err = addTorrent(a.cl, &m)
	if err != nil {
		s.idleIfUnused(uid, a)
		return nil, "", err
	}
	promActiveTorrentCount.Inc()
	e := s.track(key, func() {
		t.Drop()
		promActiveTorrentCount.Dec()
		s.releaseAccount(uid)
	})
	e.uid, e.t, e.tiers = uid, t, rec.Tiers
	e.peers = s.watch(key, t)
	go s.refreshLoop(key, h, uid, t, recordRefreshInterval)
	a.torrents++
	log.WithFields(log.Fields{"infohash": h, "acct": acct(uid)}).Info("private torrent added")
	return t, key, nil
}

// loaded returns key's torrent with its lease renewed, nil if it is not
// loaded.
func (s *TorrentMap) loaded(key string) *torrent.Torrent {
	s.mux.Lock()
	defer s.mux.Unlock()
	e, ok := s.entries[key]
	if !ok {
		return nil
	}
	e.timer.Reset(s.ttl)
	return e.t
}

// getDefault loads h into the default Client under h, as Get did before
// private records.
func (s *TorrentMap) getDefault(h string, mi *metainfo.MetaInfo) (*torrent.Torrent, string, error) {
	s.mux.Lock()
	defer s.mux.Unlock()
	cl, err := s.tc.Get()
	if err != nil {
		return nil, "", err
	}
	t, err := addTorrent(cl, mi)
	if err != nil {
		return nil, "", err
	}
	if e, ok := s.entries[h]; ok {
		e.timer.Reset(s.ttl)
		return t, h, nil
	}
	log.Infof("torrent added infohash=%v", h)
	promActiveTorrentCount.Inc()
	e := s.track(h, func() {
		t.Drop()
		promActiveTorrentCount.Dec()
	})
	e.t = t
	e.peers = s.watch(h, t)
	return t, h, nil
}

// watch starts t's watchers until it closes: chokeRedial, and every 50 ms the
// peer timeline it returns and the time-to and stall metrics.
func (s *TorrentMap) watch(key string, t *torrent.Torrent) *peerTimeline {
	go defaultChokeRedial.watch(t, func() bool { return s.reading(key) })
	startTime := time.Now()
	tl := &peerTimeline{}
	go func() {
		const tickDuration = time.Millisecond * 50
		ticker := time.NewTicker(tickDuration)
		defer ticker.Stop()
		firstPeerRecorded := false
		tenPeersRecorded := false
		thirtyPeersRecorded := false
		firstByteRecorded := false
		var lastBytesRead int64
		for {
			select {
			case <-t.Closed():
				return
			case <-ticker.C:
				stats := t.Stats()
				activePeers := stats.ActivePeers
				tl.observe(activePeers, stats.ConnectedSeeders, time.Now())
				bytesRead := stats.ConnStats.BytesRead.Int64()
				if bytesRead == lastBytesRead {
					if activePeers == 0 {
						promStallDiscoverySeconds.Add(tickDuration.Seconds())
					} else if !firstByteRecorded {
						promStallIdleSeconds.Add(tickDuration.Seconds())
					} else {
						promStallDownloadSeconds.Add(tickDuration.Seconds())
					}
				}
				lastBytesRead = bytesRead
				if !firstPeerRecorded && activePeers > 0 {
					promTimeToFirstPeerMs.Observe(float64(time.Since(startTime).Milliseconds()))
					firstPeerRecorded = true
				}
				if !tenPeersRecorded && activePeers >= 10 {
					promTimeTo10PeersMs.Observe(float64(time.Since(startTime).Milliseconds()))
					tenPeersRecorded = true
				}
				if !thirtyPeersRecorded && activePeers >= 30 {
					promTimeTo30PeersMs.Observe(float64(time.Since(startTime).Milliseconds()))
					thirtyPeersRecorded = true
				}
				if !firstByteRecorded && stats.ConnStats.BytesReadUsefulData.Int64() > 0 {
					promTimeToFirstByteMs.Observe(float64(time.Since(startTime).Milliseconds()))
					firstByteRecorded = true
				}
			}
		}
	}()
	return tl
}

func (s *TorrentMap) List() ([]string, error) {
	r := map[string]bool{}
	l, err := s.fsm.List()
	if err != nil {
		return nil, err
	}
	for _, t := range l {
		r[t] = true
	}
	rr := []string{}
	for k := range r {
		rr = append(rr, k)
	}
	sort.Strings(rr)
	return rr, nil
}
