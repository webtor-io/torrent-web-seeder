package services

import (
	"net"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
)

// chokeRedial is when a stalled torrent reconnects the peers choking it.
//
// A peer that does not upload to us is choked by many clients for as long as
// the connection lives, and a new connection gets data at once. On 2026-09-25
// a torrent with one peer got 7 s of data on a fresh connection and then
// nothing for 13+ minutes; every range not fetched in those seconds hung,
// while another pod's fresh connection to the same peer was served
// instantly. The seeder never uploads, so reconnecting is the only way back.
type chokeRedial struct {
	// How often the watcher looks.
	tick time.Duration
	// No useful data for this long, while someone is reading, is a stall.
	stall time.Duration
	// A peer choking us for this long, after it had served this connection,
	// is redialled.
	choked time.Duration
	// Not more often than this per peer IP, so a peer does not see a
	// reconnect flood.
	cooldown time.Duration
	// At most this many redials per tick, so a big swarm that chokes us
	// does not churn all at once.
	perTick int
}

var defaultChokeRedial = chokeRedial{
	tick:     10 * time.Second,
	stall:    30 * time.Second,
	choked:   30 * time.Second,
	cooldown: 2 * time.Minute,
	perTick:  5,
}

var promChokedRedials = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "torrent_web_seeder_choked_peer_redials_total",
	Help: "Connections dropped and redialled because the peer kept choking a stalled torrent that is being read",
})

func init() {
	prometheus.MustRegister(promChokedRedials)
}

// watch runs until t is closed. reading reports whether a request is being
// served from t right now; a torrent nobody reads is left alone.
func (c chokeRedial) watch(t *torrent.Torrent, reading func() bool) {
	tick := time.NewTicker(c.tick)
	defer tick.Stop()
	lastRedial := map[string]time.Time{}
	var lastBytes int64
	lastProgress := time.Now()
	for {
		select {
		case <-t.Closed():
			return
		case <-tick.C:
		}
		now := time.Now()
		stats := t.Stats()
		if b := stats.BytesReadUsefulData.Int64(); b != lastBytes {
			lastBytes = b
			lastProgress = now
			continue
		}
		if now.Sub(lastProgress) < c.stall || !reading() || t.Complete().Bool() {
			continue
		}
		n := 0
		for _, pc := range t.PeerConns() {
			if n == c.perTick {
				break
			}
			st := pc.Stats()
			if !st.PeerChoking || now.Sub(st.PeerChokingSince) < c.choked {
				continue
			}
			// Only a peer that did serve this connection is known to unchoke
			// a fresh one. One that never sent a byte chokes the new
			// connection just the same: redialling it every cooldown was most
			// of the 5.2k redials per 10 min on 2026-09-25.
			if st.BytesReadUsefulData.Int64() == 0 {
				continue
			}
			ip := peerIP(pc)
			if now.Sub(lastRedial[ip]) < c.cooldown {
				continue
			}
			lastRedial[ip] = now
			log.WithField("infohash", t.InfoHash().HexString()).WithField("peer", ip).
				WithField("choked", now.Sub(st.PeerChokingSince).Round(time.Second)).
				Info("redialling a peer that keeps choking a stalled torrent")
			pc.Redial()
			promChokedRedials.Inc()
			n++
		}
	}
}

// peerIP keys the cooldown: a peer that dialled us comes from an ephemeral
// port, and its redial goes to its listen port.
func peerIP(pc *torrent.PeerConn) string {
	a := pc.RemoteAddr.String()
	if host, _, err := net.SplitHostPort(a); err == nil {
		return host
	}
	return a
}
