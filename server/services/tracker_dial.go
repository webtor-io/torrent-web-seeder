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

// More ranges that are not on the internet and that netip has no predicate
// for: "this network" 0.0.0.0/8 (0.0.0.0 itself is unspecified), the reserved
// 240.0.0.0/4 with the limited broadcast 255.255.255.255 at its top, and the
// deprecated IPv6 site-local fec0::/10.
var (
	thisNetworkAddrs = netip.MustParsePrefix("0.0.0.0/8")
	reservedAddrs    = netip.MustParsePrefix("240.0.0.0/4")
	siteLocalAddrs   = netip.MustParsePrefix("fec0::/10")
)

// IPv6 addresses that carry an IPv4 address, which a NAT64 translator
// (RFC 6052, and RFC 8215 for local use) or a 6to4 relay (RFC 3056) connects
// to, and the deprecated IPv4-compatible form (RFC 4291). Each is judged as
// the IPv4 address it carries, read from the low 32 bits or, for 6to4, bits
// 16 to 48. A local-use translator may use a prefix shorter than /96, which
// puts the IPv4 address elsewhere; only the /96 layout is read.
var (
	nat64Addrs      = netip.MustParsePrefix("64:ff9b::/96")
	nat64LocalAddrs = netip.MustParsePrefix("64:ff9b:1::/48")
	sixToFourAddrs  = netip.MustParsePrefix("2002::/16")
	v4CompatAddrs   = netip.MustParsePrefix("::/96")
)

// embeddedIPv4 returns the IPv4 address that a, an IPv6 address without a
// zone, carries, if it is one of the forms above.
func embeddedIPv4(a netip.Addr) (netip.Addr, bool) {
	b := a.As16()
	switch {
	case nat64Addrs.Contains(a), nat64LocalAddrs.Contains(a), v4CompatAddrs.Contains(a):
		return netip.AddrFrom4([4]byte(b[12:16])), true
	case sixToFourAddrs.Contains(a):
		return netip.AddrFrom4([4]byte(b[2:6])), true
	}
	return netip.Addr{}, false
}

// nonPublicAddrClass names what kind of non-public address a is, or returns ""
// for a public one. An IPv4-mapped IPv6 address is judged as the IPv4 address
// it reaches, and so is every other IPv6 form that carries one.
func nonPublicAddrClass(a netip.Addr) string {
	// A zone does not change where an address leads, but netip's prefixes do
	// not match a zoned address, nor does IsUnspecified.
	a = a.Unmap().WithZone("")
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
	case thisNetworkAddrs.Contains(a):
		return "this-network"
	case reservedAddrs.Contains(a):
		return "reserved"
	case siteLocalAddrs.Contains(a):
		return "site-local"
	}
	// After the checks above: :: and ::1 are in the IPv4-compatible prefix.
	if v4, ok := embeddedIPv4(a); ok {
		return nonPublicAddrClass(v4)
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
	allowLocal bool
	proxyAddr  string
	warnLimit  *rate.Limiter
}

// trackerWarnLimit lets one refusal a minute be logged for the whole process.
// Each announce of a refused tracker is refused again, a .torrent can list
// many trackers, and the service builds a new client, and so a new guard,
// after an idle close; the counters carry the volume. A guard takes it when
// built, so that a test can give the guards it builds a minute of their own.
var trackerWarnLimit = rate.NewLimiter(rate.Every(time.Minute), 1)

func newTrackerGuard(allowLocal bool, proxyAddr string) *trackerGuard {
	return &trackerGuard{
		allowLocal: allowLocal,
		proxyAddr:  proxyAddr,
		warnLimit:  trackerWarnLimit,
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
		log.WithField("addr", address).WithField("class", class).Warn(
			"refused a tracker connection to a local network address (allow with --" +
				AllowLocalNetworksFlag + " or ALLOW_LOCAL_NETWORKS=true; logged at most once a minute)")
	}
	return fmt.Errorf("%w: %s is a %s address", errNonPublicAddr, address, class)
}

// dialContext is the client's TrackerDialContext: HTTP and websocket trackers.
func (g *trackerGuard) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	promDialAttempts.WithLabelValues("tracker").Inc()
	d := net.Dialer{}
	if !g.allowLocal && addr != g.proxyAddr {
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
	if err != nil || g.allowLocal {
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
