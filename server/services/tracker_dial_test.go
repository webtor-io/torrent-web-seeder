package services

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/prometheus/client_golang/prometheus/testutil"
	log "github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
	"github.com/urfave/cli"
	"golang.org/x/time/rate"
)

// A fake private-tracker key: tests check that it never reaches a log line the
// guard writes.
const fakePasskey = "0123456789abcdef0123456789abcdef"

func TestNonPublicAddrClass(t *testing.T) {
	for _, c := range []struct {
		addr, class string
	}{
		{"127.0.0.1", "loopback"},
		{"127.255.255.254", "loopback"},
		{"::1", "loopback"},
		{"10.0.0.1", "private"},
		{"172.16.0.1", "private"},
		{"172.31.255.255", "private"},
		{"192.168.0.1", "private"},
		{"fc00::1", "private"},
		{"fd12:3456::1", "private"},
		{"169.254.169.254", "link-local"},
		{"169.254.0.1", "link-local"},
		{"fe80::1", "link-local"},
		{"224.0.0.251", "multicast"},
		{"ff02::1", "multicast"},
		{"239.1.2.3", "multicast"},
		{"ff05::1:3", "multicast"},
		{"0.0.0.0", "unspecified"},
		{"::", "unspecified"},
		{"100.64.0.1", "cgnat"},
		{"100.127.255.255", "cgnat"},
		// IPv4-mapped IPv6 reaches the IPv4 address on a dual-stack socket.
		{"::ffff:127.0.0.1", "loopback"},
		{"::ffff:10.1.2.3", "private"},
		{"::ffff:169.254.169.254", "link-local"},
		{"::ffff:100.64.0.1", "cgnat"},
		{"::ffff:0.0.0.0", "unspecified"},
		{"::ffff:239.1.2.3", "multicast"},
		{"0.1.2.3", "this-network"},
		{"0.255.255.255", "this-network"},
		{"::ffff:0.1.2.3", "this-network"},
		{"240.0.0.1", "reserved"},
		{"255.255.255.254", "reserved"},
		{"255.255.255.255", "reserved"}, // limited broadcast
		{"::ffff:255.255.255.255", "reserved"},
		{"fec0::1", "site-local"},
		{"feff:ffff::1", "site-local"},
		{"febf:ffff::1", "link-local"}, // the edges of fec0::/10
		{"ff00::1", "multicast"},
		// NAT64 local-use (RFC 8215): the whole /48, whatever it carries and
		// wherever. 64:ff9b:1:a01:2:300:cb00:7107 has 10.1.2.3 at the /48
		// offset (bits 48-63 and 72-87) and a public address in the low 32 bits.
		{"64:ff9b:1::", "nat64-local-use"},
		{"64:ff9b:1::10.1.2.3", "nat64-local-use"},
		{"64:ff9b:1::203.0.113.7", "nat64-local-use"},
		{"64:ff9b:1:a01:2:300:cb00:7107", "nat64-local-use"},
		{"64:ff9b:1:ffff:ffff:ffff:ffff:ffff", "nat64-local-use"},
		// IPv6 forms that carry an IPv4 address are judged as that address.
		// NAT64, well-known prefix: the low 32 bits.
		{"64:ff9b::127.0.0.1", "loopback"},
		{"64:ff9b::10.1.2.3", "private"},
		{"64:ff9b::169.254.169.254", "link-local"},
		{"64:ff9b::100.64.0.1", "cgnat"},
		// 6to4: bits 16 to 48.
		{"2002:7f00:1::1", "loopback"},
		{"2002:a9fe:a9fe::", "link-local"},
		{"2002:c0a8:101:1::1", "private"},
		// IPv4-compatible (deprecated): the low 32 bits.
		{"::127.0.0.1", "loopback"},
		{"::10.1.2.3", "private"},
		{"::169.254.169.254", "link-local"},
		{"::240.0.0.1", "reserved"},
		// A zone does not hide the address: prefixes and IsUnspecified would
		// not match a zoned address.
		{"64:ff9b::10.1.2.3%eth0", "private"},
		{"::%eth0", "unspecified"},
		// Public: documentation ranges and the edges of the ranges above.
		{"203.0.113.7", ""},
		{"198.51.100.1", ""},
		{"2001:db8::1", ""},
		{"::ffff:203.0.113.7", ""},
		{"100.63.255.255", ""},
		{"100.128.0.0", ""},
		{"172.32.0.1", ""},
		{"11.0.0.1", ""},
		{"169.255.0.1", ""},
		{"1.0.0.1", ""},
		{"223.255.255.255", ""},
		{"64:ff9b::203.0.113.7", ""},
		{"64:ff9b::1:a01:203", ""},               // outside the /96: 10.1.2.3 in the low bits is not reached
		{"64:ff9b:0:ffff:ffff:ffff:a01:203", ""}, // just below the /48
		{"64:ff9b:2::10.1.2.3", ""},              // just above the /48
		{"2002:cb00:7107::1", ""},
		{"2003:a01:203::1", ""}, // outside 2002::/16
		{"::203.0.113.7", ""},
		{"::1:a01:203", ""}, // outside ::/96
	} {
		if got := nonPublicAddrClass(netip.MustParseAddr(c.addr)); got != c.class {
			t.Errorf("%s: class %q, want %q", c.addr, got, c.class)
		}
	}
}

// countingListener accepts TCP connections on loopback and counts them.
func countingListener(t *testing.T, network, addr string) (net.Listener, *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen(network, addr)
	if err != nil {
		t.Skipf("cannot listen on %s %s: %v", network, addr, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var n atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			n.Add(1)
			_ = c.Close()
		}
	}()
	return ln, &n
}

func trackerDialCounts() (attempts, refused, success float64) {
	return testutil.ToFloat64(promDialAttempts.WithLabelValues("tracker")),
		testutil.ToFloat64(promDialFailures.WithLabelValues("tracker", "non_public_addr")),
		testutil.ToFloat64(promDialSuccess.WithLabelValues("tracker"))
}

// The decision is made on the address the socket connects to, after DNS:
// a literal, a mapped literal and a name that resolves to loopback are all
// refused before any connection is made, and the refusal is a dial failure of
// its own category.
func TestTrackerGuard_RefusesNonPublicDials(t *testing.T) {
	ln, accepted := countingListener(t, "tcp4", "127.0.0.1:0")
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	g := newTrackerGuard(false, "")
	addrs := []string{
		"127.0.0.1:" + port,
		"[::ffff:127.0.0.1]:" + port,
		"localhost:" + port,
		"10.255.255.1:6969",
		"169.254.169.254:80",
	}
	// Without IPv6 the socket cannot be made, and the dial fails before the
	// check.
	if ln6, err := net.Listen("tcp6", "[::1]:0"); err == nil {
		_ = ln6.Close()
		addrs = append(addrs, "[::1]:"+port)
	}
	for _, addr := range addrs {
		t.Run(addr, func(t *testing.T) {
			a0, r0, s0 := trackerDialCounts()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			start := time.Now()
			conn, err := g.dialContext(ctx, "tcp", addr)
			if err == nil {
				_ = conn.Close()
				t.Fatal("dial succeeded")
			}
			if !errors.Is(err, errNonPublicAddr) {
				t.Fatalf("dial failed for another reason: %v", err)
			}
			if d := time.Since(start); d > time.Second {
				t.Errorf("refusal took %v: the address was dialled", d)
			}
			a1, r1, s1 := trackerDialCounts()
			if a1-a0 != 1 || r1-r0 != 1 || s1 != s0 {
				t.Errorf("counted attempts +%v, refusals +%v, successes +%v; want +1, +1, +0", a1-a0, r1-r0, s1-s0)
			}
		})
	}
	if n := accepted.Load(); n != 0 {
		t.Errorf("listener accepted %d connections", n)
	}
}

// A deployment whose tracker is on its own network sets the flag, and then
// nothing is refused.
func TestTrackerGuard_AllowLocalNetworksDials(t *testing.T) {
	ln, accepted := countingListener(t, "tcp4", "127.0.0.1:0")
	g := newTrackerGuard(true, "")
	_, _, s0 := trackerDialCounts()
	conn, err := g.dialContext(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial refused with the flag on: %v", err)
	}
	_ = conn.Close()
	waitFor(t, "the connection to be accepted", func() bool { return accepted.Load() == 1 })
	if _, _, s1 := trackerDialCounts(); s1-s0 != 1 {
		t.Errorf("successes +%v, want +1", s1-s0)
	}
}

// With --http-proxy every HTTP tracker request is a dial to the proxy, which
// the operator chose and which may well be on the LAN: that one address is
// dialled, any other non-public one still is not.
func TestTrackerGuard_DialsConfiguredProxy(t *testing.T) {
	proxy, accepted := countingListener(t, "tcp4", "127.0.0.1:0")
	other, otherAccepted := countingListener(t, "tcp4", "127.0.0.1:0")
	u, err := url.Parse("http://" + proxy.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	g := newTrackerGuard(false, proxyDialAddr(u))
	conn, err := g.dialContext(context.Background(), "tcp", proxy.Addr().String())
	if err != nil {
		t.Fatalf("dial to the configured proxy refused: %v", err)
	}
	_ = conn.Close()
	waitFor(t, "the proxy to accept", func() bool { return accepted.Load() == 1 })
	if _, err := g.dialContext(context.Background(), "tcp", other.Addr().String()); !errors.Is(err, errNonPublicAddr) {
		t.Errorf("dial to another loopback address: %v, want a refusal", err)
	}
	if n := otherAccepted.Load(); n != 0 {
		t.Errorf("the other listener accepted %d connections", n)
	}
}

// proxyDialAddr is the address net/http dials for a proxy URL.
func TestProxyDialAddr(t *testing.T) {
	for in, want := range map[string]string{
		"http://10.0.0.2:3128":           "10.0.0.2:3128",
		"http://proxy.invalid":           "proxy.invalid:80",
		"https://proxy.invalid":          "proxy.invalid:443",
		"socks5://proxy.invalid":         "proxy.invalid:1080",
		"http://user:pw@[fd00::1]:8080/": "[fd00::1]:8080",
	} {
		u, err := url.Parse(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := proxyDialAddr(u); got != want {
			t.Errorf("%s: %q, want %q", in, got, want)
		}
	}
}

// UDP trackers do not dial: the library resolves the host and writes to the
// address. The guard refuses the write.
func TestTrackerGuard_UDPRefusesNonPublicWrites(t *testing.T) {
	srv, got := countingUDPServer(t)
	refused := func() float64 {
		return testutil.ToFloat64(promDialFailures.WithLabelValues("udp_tracker", "non_public_addr"))
	}
	g := newTrackerGuard(false, "")
	pc, err := g.listenPacket("udp4", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	r0 := refused()
	if _, err := pc.WriteTo([]byte("x"), srv.LocalAddr()); !errors.Is(err, errNonPublicAddr) {
		t.Fatalf("write to %v: %v, want a refusal", srv.LocalAddr(), err)
	}
	if r := refused() - r0; r != 1 {
		t.Errorf("refusals +%v, want +1", r)
	}

	open, err := newTrackerGuard(true, "").listenPacket("udp4", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()
	if _, err := open.WriteTo([]byte("y"), srv.LocalAddr()); err != nil {
		t.Fatalf("write with the flag on: %v", err)
	}
	waitFor(t, "the datagram written with the flag on", func() bool { return got.Load() == 1 })
	time.Sleep(50 * time.Millisecond)
	if n := got.Load(); n != 1 {
		t.Errorf("server received %d datagrams, want only the one written with the flag on", n)
	}
}

func countingUDPServer(t *testing.T) (net.PacketConn, *atomic.Int32) {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	var n atomic.Int32
	go func() {
		b := make([]byte, 2048)
		for {
			if _, _, err := pc.ReadFrom(b); err != nil {
				return
			}
			n.Add(1)
		}
	}()
	return pc, &n
}

// An address the guard cannot read is not taken for a public one.
func TestTrackerGuard_RefusesUnparsableAddr(t *testing.T) {
	g := newTrackerGuard(false, "")
	for _, a := range []string{"", "tracker.invalid:80", "203.0.113.7"} {
		if err := g.check(a); !errors.Is(err, errNonPublicAddr) {
			t.Errorf("check(%q): %v, want a refusal", a, err)
		}
	}
	if err := g.check("203.0.113.7:80"); err != nil {
		t.Errorf("check of a public address: %v", err)
	}
}

// A refusal is logged at most once a minute for the whole process, naming the
// address, its class and the variable that allows it: each announce of a
// refused tracker is refused again, and the service builds a new client (and
// guard) after an idle close.
func TestTrackerGuard_WarnsOnceAMinute(t *testing.T) {
	freshWarnLimit(t)
	hook := test.NewGlobal()
	defer hook.Reset()
	for _, g := range []*trackerGuard{newTrackerGuard(false, ""), newTrackerGuard(false, "")} {
		for range 3 {
			if err := g.check("10.1.2.3:6969"); !errors.Is(err, errNonPublicAddr) {
				t.Fatalf("check: %v", err)
			}
		}
	}
	var warns []*log.Entry
	for _, e := range hook.AllEntries() {
		if e.Level == log.WarnLevel {
			warns = append(warns, e)
		}
	}
	if len(warns) != 1 {
		t.Fatalf("%d warnings for 6 refusals by 2 guards, want 1", len(warns))
	}
	line, err := warns[0].String()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"10.1.2.3:6969", "private", "--allow-local-networks", "ALLOW_LOCAL_NETWORKS=true"} {
		if !strings.Contains(line, want) {
			t.Errorf("warning %q does not name %q", line, want)
		}
	}
}

// freshWarnLimit gives the guards built during the test a minute of their own:
// the limit is the process's, and an earlier test may have used it up.
func freshWarnLimit(t *testing.T) {
	old := trackerWarnLimit
	trackerWarnLimit = rate.NewLimiter(rate.Every(time.Minute), 1)
	t.Cleanup(func() { trackerWarnLimit = old })
}

// trackerTestConfig is loopbackConfig with trackers on.
func trackerTestConfig(cfg *torrent.ClientConfig) {
	loopbackConfig(cfg)
	cfg.DisableTrackers = false
}

// The client the service builds announces through the guard, HTTP and UDP: a
// torrent whose trackers are on loopback reaches neither of them, the refusals
// are counted, and the line logged carries no part of the announce URL. With
// the flag on, both get the announce.
func TestTorrentClient_TrackerGuard(t *testing.T) {
	for _, allow := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "allow-local-networks"}[allow], func(t *testing.T) {
			freshWarnLimit(t)
			hook := test.NewGlobal()
			defer hook.Reset()
			var httpGot atomic.Int32
			hsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				httpGot.Add(1)
				_, _ = io.WriteString(w, trackerAnswers)
			}))
			t.Cleanup(hsrv.Close)
			usrv, udpGot := countingUDPServer(t)

			tc := &TorrentClient{clientSettings: clientSettings{
				swarm:              newSwarmStats(),
				testConfig:         trackerTestConfig,
				maxUnverifiedBytes: -1,
				dataDir:            t.TempDir(),
				allowLocalNetworks: allow,
			}}
			t.Cleanup(tc.Close)
			cl, err := tc.Get()
			if err != nil {
				t.Fatal(err)
			}
			_, mi := addTestPayload(t)
			mi.AnnounceList = metainfo.AnnounceList{
				{hsrv.URL + "/announce?passkey=" + fakePasskey},
				{"udp://" + usrv.LocalAddr().String() + "/announce/" + fakePasskey},
			}
			refusedTCP := func() float64 {
				return testutil.ToFloat64(promDialFailures.WithLabelValues("tracker", "non_public_addr"))
			}
			refusedUDP := func() float64 {
				return testutil.ToFloat64(promDialFailures.WithLabelValues("udp_tracker", "non_public_addr"))
			}
			t0, u0 := refusedTCP(), refusedUDP()
			if _, err := addTorrent(cl, mi); err != nil {
				t.Fatal(err)
			}

			if allow {
				waitFor(t, "both trackers to get an announce", func() bool {
					return httpGot.Load() > 0 && udpGot.Load() > 0
				})
				if d, u := refusedTCP()-t0, refusedUDP()-u0; d != 0 || u != 0 {
					t.Errorf("refusals counted with the flag on: tcp +%v, udp +%v", d, u)
				}
				return
			}
			waitFor(t, "both announces to be refused", func() bool {
				return refusedTCP() > t0 && refusedUDP() > u0
			})
			time.Sleep(100 * time.Millisecond)
			if h, u := httpGot.Load(), udpGot.Load(); h != 0 || u != 0 {
				t.Errorf("trackers on loopback were reached: http %d, udp %d", h, u)
			}
			var warned bool
			for _, e := range hook.AllEntries() {
				if !strings.Contains(e.Message, "refused a tracker") {
					continue
				}
				warned = true
				line, err := e.String()
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(line, fakePasskey) || strings.Contains(line, "announce") || strings.Contains(line, "://") {
					t.Errorf("the refusal line carries the announce URL: %q", line)
				}
			}
			if !warned {
				t.Error("no refusal was logged")
			}
		})
	}
}

// With --http-proxy on the local network, HTTP announces go to the proxy and
// are not refused. The tracker's documentation address is never dialled: the
// proxy here answers for it.
func TestTorrentClient_TrackerViaLocalProxy(t *testing.T) {
	var proxied atomic.Int32
	psrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host == "203.0.113.7:6969" {
			proxied.Add(1)
		}
		_, _ = io.WriteString(w, trackerAnswers)
	}))
	t.Cleanup(psrv.Close)
	tc := &TorrentClient{clientSettings: clientSettings{
		swarm:              newSwarmStats(),
		testConfig:         trackerTestConfig,
		maxUnverifiedBytes: -1,
		dataDir:            t.TempDir(),
		proxy:              psrv.URL,
	}}
	t.Cleanup(tc.Close)
	cl, err := tc.Get()
	if err != nil {
		t.Fatal(err)
	}
	_, mi := addTestPayload(t)
	mi.AnnounceList = metainfo.AnnounceList{{"http://203.0.113.7:6969/announce?passkey=" + fakePasskey}}
	if _, err := addTorrent(cl, mi); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the announce through the proxy", func() bool { return proxied.Load() > 0 })
}

// The flag and its variable reach the client; without either nothing is
// allowed. The names are spelled out: the variable is shared with
// magnet2torrent through one env file in self-hosted.
func TestNewTorrentClient_AllowLocalNetworks(t *testing.T) {
	parse := func(args ...string) bool {
		t.Helper()
		var got bool
		app := cli.NewApp()
		app.Flags = RegisterTorrentClientFlags(nil)
		app.Action = func(c *cli.Context) error {
			tc, err := NewTorrentClient(c)
			if err != nil {
				return err
			}
			got = tc.allowLocalNetworks
			return nil
		}
		if err := app.Run(append([]string{"seeder"}, args...)); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if parse() {
		t.Error("allowed by default")
	}
	if !parse("--allow-local-networks") {
		t.Error("the flag does not allow")
	}
	t.Setenv("ALLOW_LOCAL_NETWORKS", "true")
	if !parse() {
		t.Error("the variable does not allow")
	}
}

// categorizeError keeps refusals apart from network failures however they are
// wrapped on the way up.
func TestCategorizeError_NonPublicAddr(t *testing.T) {
	err := &net.OpError{Op: "dial", Net: "tcp", Err: fmt.Errorf("%w: 10.1.2.3:80 is a private address", errNonPublicAddr)}
	if got := categorizeError(err); got != "non_public_addr" {
		t.Errorf("category %q, want non_public_addr", got)
	}
	if got := categorizeError(&url.Error{Op: "Get", URL: "http://x.invalid", Err: err}); got != "non_public_addr" {
		t.Errorf("category %q through url.Error, want non_public_addr", got)
	}
}
