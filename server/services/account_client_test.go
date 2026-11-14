package services

import (
	"testing"

	"github.com/anacrolix/torrent"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urfave/cli"
	"golang.org/x/time/rate"
)

// torrentClientFromFlags builds a TorrentClient the way the service does, through
// NewTorrentClient and the registered flags (pattern: TestNewTorrentClient_AllowLocalNetworks).
func torrentClientFromFlags(t *testing.T, args ...string) *TorrentClient {
	t.Helper()
	var tc *TorrentClient
	app := cli.NewApp()
	app.Flags = RegisterTorrentClientFlags(nil)
	app.Action = func(c *cli.Context) (err error) {
		tc, err = NewTorrentClient(c)
		return err
	}
	if err := app.Run(append([]string{"seeder"}, args...)); err != nil {
		t.Fatal(err)
	}
	return tc
}

type cfgSeen struct {
	noDHT, disablePEX, noPortForwarding bool
	listenPort                          int
	ua                                  string
	download, dial                      *rate.Limiter
}

// seeing records what a Client is built with, then points it at loopback.
func seeing(dst *cfgSeen) func(*torrent.ClientConfig) {
	return func(cfg *torrent.ClientConfig) {
		*dst = cfgSeen{
			noDHT:            cfg.NoDHT,
			disablePEX:       cfg.DisablePEX,
			noPortForwarding: cfg.NoDefaultPortForwarding,
			listenPort:       cfg.ListenPort,
			ua:               cfg.HTTPUserAgent,
			download:         cfg.DownloadRateLimiter,
			dial:             cfg.DialRateLimiter,
		}
		loopbackConfig(cfg)
	}
}

func TestForUser_AccountSettings(t *testing.T) {
	tc := torrentClientFromFlags(t, "--user-agent", "Transmission/2.94", "--data-dir", t.TempDir())
	acc := tc.forUser() // before the default Client is built: forUser must not change tc
	var def, got cfgSeen
	tc.testConfig = seeing(&def)
	if _, err := tc.Get(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tc.Close)
	acc.testConfig = seeing(&got)
	cl, err := acc.get()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })
	lib := torrent.NewDefaultClientConfig()
	if def.ua != "Transmission/2.94" || def.noDHT || def.disablePEX || def.listenPort != lib.ListenPort {
		t.Errorf("default Client changed: %+v", def)
	}
	if !got.noDHT || !got.disablePEX || !got.noPortForwarding || got.listenPort != 0 || got.ua != lib.HTTPUserAgent {
		t.Errorf("account Client = %+v, want NoDHT, DisablePEX, NoDefaultPortForwarding, port 0, library UA %q", got, lib.HTTPUserAgent)
	}
	if acc.clientIdleTimeout != 0 || tc.clientIdleTimeout == 0 {
		t.Errorf("idle timeout: account %v, default %v; want 0 and the flag's", acc.clientIdleTimeout, tc.clientIdleTimeout)
	}
}

// Two accounts' Clients and the default one draw on one dial and one download
// budget: the pod's ceiling does not grow with the number of accounts. Without
// DIAL_RATE_LIMIT the library would give each Client its own 10/s limiter.
func TestAccountClientsShareLimiters(t *testing.T) {
	for _, c := range []struct {
		args     []string
		dial     rate.Limit
		download bool
	}{
		{[]string{"--download-rate", "1M", "--dial-rate-limit", "5"}, 5, true},
		{nil, torrent.NewDefaultClientConfig().DialRateLimiter.Limit(), false},
	} {
		tc := torrentClientFromFlags(t, append(c.args, "--data-dir", t.TempDir())...)
		var seen [3]cfgSeen
		for i, cc := range []*TorrentClient{tc, tc.forUser(), tc.forUser()} {
			cc.testConfig = seeing(&seen[i])
			cl, err := cc.get()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { cl.Close() })
		}
		for i := range seen {
			if seen[i].dial == nil || seen[i].dial != seen[0].dial || (c.download && (seen[i].download == nil || seen[i].download != seen[0].download)) {
				t.Fatalf("%v: Client %d has its own limiters: %+v vs %+v", c.args, i, seen[i], seen[0])
			}
		}
		if seen[0].dial.Limit() != c.dial {
			t.Fatalf("%v: dial limit %v, want %v", c.args, seen[0].dial.Limit(), c.dial)
		}
	}
}

func TestConnGauge_SumsClients(t *testing.T) {
	est0, half0 := testutil.ToFloat64(promEstablishedConns), testutil.ToFloat64(promHalfOpenConns)
	var a, b connGauge
	a.set(3, 1)
	b.set(4, 2)
	a.set(1, 0)
	if e, h := testutil.ToFloat64(promEstablishedConns)-est0, testutil.ToFloat64(promHalfOpenConns)-half0; e != 5 || h != 2 {
		t.Fatalf("gauges +%v/+%v, want +5/+2 (sum of the Clients' last values)", e, h)
	}
	a.retire()
	if e := testutil.ToFloat64(promEstablishedConns) - est0; e != 4 {
		t.Fatalf("after retire +%v, want +4", e)
	}
}

// The metrics loop of each Client adds its own share: two Clients with a peer
// each show as two, and a Client that closes takes its share out. The peer and
// both Clients want the payload and hold none of it, so the connections stay
// up and nothing completes; the peer sends no PEX, so the Clients do not meet.
func TestConnGauge_ClientsLoopsSum(t *testing.T) {
	est0 := testutil.ToFloat64(promEstablishedConns)
	established := func(want float64) func() bool {
		return func() bool { return testutil.ToFloat64(promEstablishedConns)-est0 == want }
	}
	_, mi := addTestPayload(t)
	pcfg := addTestConfig(t.TempDir())
	pcfg.DisablePEX = true
	peer, err := torrent.NewClient(pcfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { peer.Close() })
	ptor, err := peer.AddTorrent(mi)
	if err != nil {
		t.Fatal(err)
	}
	ptor.DownloadAll()
	tc := &TorrentClient{clientSettings: clientSettings{
		swarm:              newSwarmStats(),
		testConfig:         loopbackConfig,
		maxUnverifiedBytes: -1,
		dataDir:            t.TempDir(),
	}}
	acc := tc.forUser()
	acc.dataDir = t.TempDir()
	var cls []*torrent.Client
	for _, cc := range []*TorrentClient{tc, acc} {
		cl, err := cc.get()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cl.Close() })
		tor, err := addTorrent(cl, mi)
		if err != nil {
			t.Fatal(err)
		}
		tor.DownloadAll()
		tor.AddClientPeer(peer)
		cls = append(cls, cl)
	}
	waitFor(t, "both Clients' peers in the gauge", established(2))
	cls[1].Close()
	waitFor(t, "the closed Client's share to leave", established(1))
	cls[0].Close()
	waitFor(t, "the gauge back where it was", established(0))
}
