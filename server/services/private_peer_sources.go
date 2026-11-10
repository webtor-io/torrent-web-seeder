package services

import (
	"github.com/anacrolix/torrent"
	"github.com/prometheus/client_golang/prometheus"
)

// Where the peers of private torrents come from, and how much of their data
// each source brings.
//
// BEP 27 says a private torrent gets its peers from its trackers only, but the
// library also announces it to the DHT and exchanges it over PEX. Before that
// is switched off the counters show what it would cost: if the DHT and PEX
// peers of private torrents bring a real share of their data, turning them off
// slows those torrents down by that share.
//
// Both are counted from the library's callbacks, under the client lock, with
// what the library records on each connection: Peer.Discovery is set before
// the connection is added to its torrent and does not change after.
// ReceivedUsefulData fires for every chunk the torrent still needed, right
// where the library adds it to BytesReadUsefulData, so the bytes add up to the
// "useful" kind of torrent_web_seeder_peer_read_bytes_total for the private
// torrents' share. A pass over the torrents' connections would miss what a
// connection got between the last pass and its close. A connection is counted
// when the library adds it to its torrent (PeerConnAdded), after the
// handshakes. A torrent whose info is not known yet cannot be told private and
// is not counted; the service adds torrents from their metainfo, info included.

var peerSourceLabels = []string{"tracker", "dht", "pex", "incoming", "other"}

var (
	promPrivatePeerUsefulBytes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "torrent_web_seeder_private_peer_useful_bytes_total",
		Help: "Piece payload that was needed, received from peers of private (BEP 27) torrents, by how the peer was found: " +
			"tracker, dht, pex, incoming (it connected to us), other (given directly, holepunch, web seed).",
	}, []string{"source"})
	promPrivatePeerConns = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "torrent_web_seeder_private_peer_connections_total",
		Help: "Peer connections established for private (BEP 27) torrents, by how the peer was found; " +
			"labels as torrent_web_seeder_private_peer_useful_bytes_total.",
	}, []string{"source"})

	// Each label's counter, looked up once: the bytes one is added to for
	// every useful chunk of a private torrent. Building them also exports
	// every label at zero from the start.
	privateUsefulBytesBySource = map[string]prometheus.Counter{}
	privateConnsBySource       = map[string]prometheus.Counter{}
)

func init() {
	prometheus.MustRegister(promPrivatePeerUsefulBytes)
	prometheus.MustRegister(promPrivatePeerConns)
	for _, l := range peerSourceLabels {
		privateUsefulBytesBySource[l] = promPrivatePeerUsefulBytes.WithLabelValues(l)
		privateConnsBySource[l] = promPrivatePeerConns.WithLabelValues(l)
	}
}

// peerSourceLabel maps the library's record of how a peer was found to one of
// peerSourceLabels.
func peerSourceLabel(s torrent.PeerSource) string {
	switch s {
	case torrent.PeerSourceTracker:
		// A WebRTC peer we offered to through a websocket tracker too.
		return "tracker"
	case torrent.PeerSourceDhtGetPeers, torrent.PeerSourceDhtAnnouncePeer:
		return "dht"
	case torrent.PeerSourcePex:
		return "pex"
	case torrent.PeerSourceIncoming:
		return "incoming"
	}
	return "other"
}

// torrentIsPrivate reads the BEP 27 flag. Torrent.Info takes only the
// torrent's name lock, which nothing holds while taking the client lock, so it
// is safe from a callback that runs under the client lock. Both callbacks get
// a connection the library has already attached to its torrent, so t is
// never nil there.
func torrentIsPrivate(t *torrent.Torrent) bool {
	info := t.Info()
	return info != nil && info.Private != nil && *info.Private
}

// countPrivatePeerConn is a Callbacks.PeerConnAdded.
func countPrivatePeerConn(pc *torrent.PeerConn) {
	if torrentIsPrivate(pc.Torrent()) {
		privateConnsBySource[peerSourceLabel(pc.Discovery)].Inc()
	}
}

// countPrivateUsefulData is a Callbacks.ReceivedUsefulData.
func countPrivateUsefulData(e torrent.ReceivedUsefulDataEvent) {
	if torrentIsPrivate(e.Peer.Torrent()) {
		privateUsefulBytesBySource[peerSourceLabel(e.Peer.Discovery)].Add(float64(len(e.Message.Piece)))
	}
}
