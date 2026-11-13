package services

import (
	"net/http"
	"reflect"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
)

// accountIdleTimeout is how long an account's Client outlives its last
// torrent.
var accountIdleTimeout = 2 * time.Minute

// maxAccountClients is a constant until account Clients are measured in
// production: no flag, no env var.
// It is a var only so that a test can lower it.
var maxAccountClients = 100

var (
	promAccountClients = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "torrent_web_seeder_account_clients",
		Help: "Account Clients the pod holds.",
	})
	promAccountClientsRefused = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "torrent_web_seeder_account_clients_refused_total",
		Help: "Requests of a new account answered busy: the pod holds as many account Clients as it may.",
	})
)

func init() {
	prometheus.MustRegister(promAccountClients, promAccountClientsRefused)
}

// accountClient is one account's BitTorrent Client and the number of its
// torrents the map holds.
type accountClient struct {
	cl       *torrent.Client
	torrents int
	idle     *time.Timer
}

// accountFor returns uid's Client, built from the pod's settings (forUser) on
// the account's first torrent; a failed build is not kept, the next request
// builds again. A new account past maxAccountClients is refused busy. Called
// with s.mux held.
func (s *TorrentMap) accountFor(uid string) (*accountClient, error) {
	if a, ok := s.accounts[uid]; ok {
		if a.idle != nil {
			a.idle.Stop()
		}
		return a, nil
	}
	if len(s.accounts) >= maxAccountClients {
		promAccountClientsRefused.Inc()
		return nil, &seederRefusal{http.StatusServiceUnavailable, reasonBusy}
	}
	cl, err := s.tc.forUser().get()
	if err != nil {
		return nil, err
	}
	a := &accountClient{cl: cl}
	s.accounts[uid] = a
	promAccountClients.Inc()
	return a, nil
}

// releaseAccount counts out one of uid's torrents. Called with s.mux held.
func (s *TorrentMap) releaseAccount(uid string) {
	if a, ok := s.accounts[uid]; ok {
		a.torrents--
		s.idleIfUnused(uid, a)
	}
}

// idleIfUnused closes a after accountIdleTimeout if it holds no torrent.
// Called with s.mux held.
func (s *TorrentMap) idleIfUnused(uid string, a *accountClient) {
	if a.torrents == 0 {
		a.idle = time.AfterFunc(accountIdleTimeout, func() { s.closeIdleAccount(uid, a) })
	}
}

// closeIdleAccount closes a, uid's Client, unless a torrent was added to it
// or it was replaced since its idle timer was set. The check and the removal
// are under s.mux, so a request never gets a torrent of a closed Client.
func (s *TorrentMap) closeIdleAccount(uid string, a *accountClient) {
	s.mux.Lock()
	if s.accounts[uid] != a || a.torrents != 0 {
		s.mux.Unlock()
		return
	}
	delete(s.accounts, uid)
	promAccountClients.Dec()
	s.mux.Unlock()
	a.cl.Close()
	s.tc.swarm.retire(a.cl)
	log.WithField("acct", acct(uid)).Info("account client closed")
}

// CloseAccounts unloads every account's torrents (Drop sends stopped with that
// account's key), waits wait, then closes the account Clients. Returns at once when
// there is none: most pods have no account Client, and shutdown must not wait
// for nothing. The default Client is not touched.
//
// The idle closes of the accounts are stopped, those the unloads arm and any
// armed before: a timer would keep its closed Client and that Client's
// torrents reachable for accountIdleTimeout.
func (s *TorrentMap) CloseAccounts(wait time.Duration) {
	s.mux.Lock()
	if len(s.accounts) == 0 {
		s.mux.Unlock()
		return
	}
	for key, e := range s.entries {
		if e.uid != "" {
			s.unload(key, e)
		}
	}
	accounts := s.accounts
	s.accounts = map[string]*accountClient{}
	for _, a := range accounts {
		if a.idle != nil {
			a.idle.Stop()
		}
	}
	promAccountClients.Sub(float64(len(accounts)))
	s.mux.Unlock()
	log.Infof("closing %d account clients in %v", len(accounts), wait)
	time.Sleep(wait)
	for _, a := range accounts {
		a.cl.Close()
		s.tc.swarm.retire(a.cl)
	}
}

// recordRefreshInterval is how often an account's loaded torrent re-reads its
// record. A var only so that a test can shorten it.
var recordRefreshInterval = 60 * time.Second

// refreshLoop re-reads (h, uid) past the cache every interval until t closes.
// A record gone (missing) or with other trackers unloads the torrent, even
// while it is being read: Drop sends stopped with the old key, and the next
// request adds it again from the record the tick left in the cache. An
// unavailable, disabled or bad answer leaves the torrent and its trackers as
// they are. The loop ends when key no longer holds t.
//
// Get passes recordRefreshInterval: read there rather than in the loop's own
// goroutine, it does not race with a test that restores the var.
func (s *TorrentMap) refreshLoop(key, h, uid string, t *torrent.Torrent, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-t.Closed():
			return
		case <-ticker.C:
		}
		rec, outcome := s.records.Refresh(h, uid)
		s.mux.Lock()
		e := s.entries[key]
		if e == nil || e.t != t {
			s.mux.Unlock()
			return
		}
		var reason string
		switch {
		case outcome == recordMissing:
			reason = "gone"
		case outcome == recordOK && !reflect.DeepEqual(rec.Tiers, e.tiers):
			reason = "changed"
		default:
			s.mux.Unlock()
			continue
		}
		s.unload(key, e)
		s.mux.Unlock()
		log.WithFields(log.Fields{"infohash": h, "acct": acct(uid), "reason": reason}).Info("private torrent unloaded")
		return
	}
}
