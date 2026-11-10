package services

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/time/rate"
)

// errNonPublicAddr is a tracker connection refused by trackerGuard;
// categorizeError counts it as non_public_addr.
var errNonPublicAddr = errors.New("tracker address is not public")

// cgnatAddrs is the shared address space of RFC 6598 (carrier-grade NAT).
// netip has no predicate for it, and clusters and VPNs number their own
// networks out of it.
var cgnatAddrs = netip.MustParsePrefix("100.64.0.0/10")

// nonPublicAddrClass names what kind of non-public address a is, or returns ""
// for a public one. An IPv4-mapped IPv6 address is judged as the IPv4 address
// it reaches.
func nonPublicAddrClass(a netip.Addr) string {
	a = a.Unmap()
	switch {
	case a.IsLoopback():
		return "loopback"
	case a.IsPrivate(): // RFC 1918, and fc00::/7 for IPv6
		return "private"
	case a.IsLinkLocalUnicast(): // 169.254.169.254, the cloud metadata address, is one
		return "link-local"
	case a.IsUnspecified():
		return "unspecified"
	case a.IsMulticast(): // link-local multicast included
		return "multicast"
	case cgnatAddrs.Contains(a):
		return "cgnat"
	}
	return ""
}

// trackerGuard keeps tracker connections off non-public addresses.
//
// Anyone can hand the service a .torrent, and the pod announces to every
// tracker in it, once a minute while an announce keeps failing. Without a check
// a tracker on loopback, on a cluster service's address or on the cloud
// metadata address gets a request from inside the cluster, with a path and
// query of the author's choosing. The address is checked where the socket
// connects, in net.Dialer.Control, which runs after name resolution, for each
// address tried. So a name that resolves to a private address, a redirect to
// one (net/http dials every hop through the same DialContext) and a name that
// resolves differently by the time of the dial are all judged by the address
// actually connected to. Websocket trackers dial through the same function.
//
// UDP trackers do not dial: the library resolves the host itself and writes
// every request to the address it got (tracker/udp clientWriter), so for them
// the check is on WriteTo.
//
// With --http-proxy every HTTP request is a connection to the proxy, which the
// operator chose and which is often on the local network, so that one address
// is not checked; where the proxy connects on is its own policy.
type trackerGuard struct {
	allowPrivate bool
	proxyAddr    string
	// One refusal a minute is logged. Each announce of a refused tracker is
	// refused again, and a .torrent can list many trackers; the counters
	// carry the volume.
	warnLimit *rate.Limiter
}

func newTrackerGuard(allowPrivate bool, proxyAddr string) *trackerGuard {
	return &trackerGuard{
		allowPrivate: allowPrivate,
		proxyAddr:    proxyAddr,
		warnLimit:    rate.NewLimiter(rate.Every(time.Minute), 1),
	}
}

// proxyDialAddr is the address net/http dials for the proxy u: its host and
// port, the port defaulting by scheme (canonicalAddr in net/http). The name is
// compared as written; a non-ASCII one, which net/http dials in punycode, does
// not match and is then checked like a tracker address.
func proxyDialAddr(u *url.URL) string {
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		case "socks5", "socks5h":
			port = "1080"
		}
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// check refuses address, an ip:port as the socket layer prints it, unless it
// is public. An address it cannot parse is refused too.
//
// The line logged names the address connected to and the flag that allows
// it, and nothing of the tracker URL: a private tracker's passkey is in its
// path or query.
func (g *trackerGuard) check(address string) error {
	class := "unparsable"
	if ap, err := netip.ParseAddrPort(address); err == nil {
		class = nonPublicAddrClass(ap.Addr())
	}
	if class == "" {
		return nil
	}
	if g.warnLimit.Allow() {
		log.WithField("addr", address).WithField("class", class).Warnf(
			"refused a tracker connection to a non-public address; set --%s (TRACKER_ALLOW_PRIVATE_ADDRS=true) if the tracker is on the local network",
			TrackerAllowPrivateAddrsFlag)
	}
	return fmt.Errorf("%w: %s is a %s address", errNonPublicAddr, address, class)
}

// dialContext is the client's TrackerDialContext: HTTP and websocket trackers.
func (g *trackerGuard) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	promDialAttempts.WithLabelValues("tracker").Inc()
	d := net.Dialer{}
	if !g.allowPrivate && addr != g.proxyAddr {
		d.Control = func(_, address string, _ syscall.RawConn) error {
			return g.check(address)
		}
	}
	conn, err := d.DialContext(ctx, network, addr)
	if err != nil {
		promDialFailures.WithLabelValues("tracker", categorizeError(err)).Inc()
	} else {
		promDialSuccess.WithLabelValues("tracker").Inc()
	}
	return conn, err
}

// listenPacket is the client's TrackerListenPacket: the socket of one UDP
// tracker announce.
func (g *trackerGuard) listenPacket(network, addr string) (net.PacketConn, error) {
	pc, err := net.ListenPacket(network, addr)
	if err != nil || g.allowPrivate {
		return pc, err
	}
	return &trackerPacketConn{PacketConn: pc, g: g}, nil
}

// trackerPacketConn refuses to write to a non-public address. The library
// gives up the announce on the first write error, so each refused announce
// counts once, as a udp_tracker dial failure: there is no dial to count as an
// attempt.
type trackerPacketConn struct {
	net.PacketConn
	g *trackerGuard
}

func (c *trackerPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	if err := c.g.check(addr.String()); err != nil {
		promDialFailures.WithLabelValues("udp_tracker", categorizeError(err)).Inc()
		return 0, err
	}
	return c.PacketConn.WriteTo(p, addr)
}
