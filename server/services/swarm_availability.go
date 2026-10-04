package services

import (
	"cmp"
	"slices"
	"time"

	"github.com/RoaringBitmap/roaring"
)

// Swarm availability: which of a stat's pieces the seeder could get at all.
// A torrent with peers connected can still be stuck because every peer holds
// a partial copy and nobody holds the pieces being read; the status bar then
// shows peers and no progress, and users ask why. These numbers let web-ui
// hatch the pieces nobody connected has announced and say how much of the
// torrent (or of the file or directory asked about) is here or announced by
// a connected peer — not by the peers alone: pieces complete here count.
//
// The computation (computeAvailability) is pure; the glue that reads the
// torrent (readSwarm) stays thin and hands it one union of the peers' piece
// sets, so that reading availability from the library's own per-piece
// counters instead would change only the glue.

// availabilitySettle is how long a torrent must have had connected peers,
// without a break, before the union of their piece sets is reported as what
// they have. It is a product delay, chosen to cover the first seconds' burst
// of connections; it is not derived from a protocol timeout. What the fork
// (webtor-io/torrent) does that makes a delay necessary:
//
//   - A connection joins Torrent.conns — PeerConns, ActivePeers — right after
//     the BitTorrent handshake (runHandshookConn → addPeerConn), before the
//     peer's first message is read. Its piece set is empty until that
//     message: BITFIELD (peerSentBitfield), HAVE_ALL (onPeerSentHaveAll) or
//     HAVE_NONE (peerSentHaveNone). A peer with nothing may skip the bitfield
//     (BEP 3), and "lazy bitfield" clients send part of it as HAVEs after
//     (peerSentHave, one bit each). So a fresh connection under-reports for
//     about one round trip; prod's handshake timeout is 3 s, and a peer that
//     answers slower than that is not connected at all.
//   - In the first seconds peers arrive in a burst from the first tracker and
//     DHT answers, each one silent for its round trip, and the union taken
//     then paints holes that the next second fills.
//
// The clock is the torrent's, not a connection's: TorrentMap's watcher
// records since when the torrent has had at least one peer and no connected
// seeder (peerTimeline), so a chain of short-lived connections counts as one
// streak, a gap of no peer shorter than peerGapGrace too, and a seeder
// leaving — perhaps only for a redial — starts a new one. The library does
// expose per-connection moments — Callbacks.CompletedHandshake and
// PeerConnAdded for when a connection was made, Callbacks.ReadMessage for its
// first message — and "every connection has sent its first message" would be
// the exact rule. It is not used:
// ReadMessage runs under the client lock for every message on every
// connection, the data path of every stream, to answer a question asked once
// a second by a status page; and under churn at the connection limit there
// is almost always a connection younger than a round trip, so the exact rule
// would stay unknown exactly when a swarm is stuck and cycling peers. The
// cost of the torrent clock: a peer that joins after the streak started is
// unread for its round trip; it can only add pieces, so it can only paint a
// hole that is not there, until a reply computed from a later snapshot goes
// out (up to ~3 s: the swarm snapshot is kept 1 s, and a lone StatStream
// gets a fresh reply every ~2 s — see Stat.cache).
//
// "Known" says the connected peers have announced what they have, not that
// the swarm has been found: peers keep arriving after 20 s (prod, 7 d to
// 2026-09-24: first peer p50 3.9 s after add, ten peers p50 37 s), and
// availability rises as they do. It also cannot see a peer that hides what
// it has: a BEP 16 super-seeder announces an empty set and reveals pieces
// one HAVE at a time, so a full copy behind it looks like holes that fill as
// the data arrives. Missing means "no connected peer has announced it".
//
// Settled at once when a source claims every piece: ConnectedSeeders counts
// a connection whose claim to every piece is known (HAVE_ALL or a full
// bitfield; Torrent.gauges), and nothing a peer sends later adds to "all";
// a web seed claims every piece of a torrent it can serve (readSwarm). And
// settled when nothing in the scope is missing: pieces complete here or
// already announced cannot become unavailable by more announcements.
const availabilitySettle = 20 * time.Second

// maxMissingRanges caps StatReply.missing. Past it, the ranges separated by
// the smallest gaps are merged (capRanges), so a merged range covers pieces
// that are not missing: complete here or announced by a peer — how many is
// not bounded (a 64k-piece torrent with ~8k scattered holes merges gaps of
// up to ~20 pieces). Clients draw completion over the hatch. The cap is
// twice the web-ui piece bar's 256 buckets, so a merged gap is mostly below
// a bucket, and it bounds `missing` in an SSE frame at ~14.5 KB (~28 bytes
// a range); StatStream sends it only in frames where it changed.
const maxMissingRanges = 512

// pieceRange is a half-open run [start, end) of piece positions, relative to
// the stat's scope.
type pieceRange struct {
	start, end int
}

// availabilityInput is what a stat knows about one scope (the torrent, a
// file, a directory's piece span) and the swarm.
type availabilityInput struct {
	// offset is the torrent index of the scope's first piece: peer piece
	// sets are torrent-indexed, the result is scope-relative.
	offset int
	// complete holds, per scope piece, whether it is complete here. Its
	// length is the scope's piece count.
	complete []bool
	// wanted holds, per scope piece, whether its priority is above none —
	// sticky priorities included (warm-up's High, linger's Normal).
	// Shorter than complete (or nil) means the rest is not wanted.
	wanted []bool
	// reading holds, per scope piece, whether an open reader is on it or
	// reading ahead into it (priority Readahead or above). Shorter than
	// complete (or nil) means the rest is not.
	reading []bool
	// peers are torrent-indexed piece sets whose union is what the connected
	// peers announce: readSwarm passes that union, tests may pass one set per
	// peer. Only read, never changed: one snapshot serves concurrent stats.
	// Not read when seeders > 0.
	peers []*roaring.Bitmap
	// connected is the number of connected peers the sets were read from.
	connected int
	// seeders is the number of sources that claim every piece: connected
	// seeders, and web seeds serving the whole torrent.
	seeders int
	// partialWebseeds is the number of web seeds that claim some pieces but
	// not all. Which ones is not readable through the library's API, so they
	// make availability unknown unless nothing is missing anyway.
	partialWebseeds int
	// peersFor is how long the torrent has had connected peers and no
	// seeder without a break (peerTimeline.connectedFor).
	peersFor time.Duration
}

// availability is StatReply's swarm fields for one scope.
type availability struct {
	// fraction of the scope's pieces complete here or on a connected peer;
	// a lower bound while !known.
	fraction float32
	known    bool
	// missing are the scope's pieces neither here nor on any connected peer.
	// Empty unless known.
	missing []pieceRange
	// wantedMissing counts wanted, incomplete pieces without a source. Zero
	// unless known.
	wantedMissing int
	// readerMissing counts the pieces among those that an open reader is on
	// or reading ahead into. Zero unless known.
	readerMissing int
}

// computeAvailability is the swarm availability of one scope. It costs
// O(containers + holes) on the peer side: the scope's pieces on no peer come
// from roaring set operations, and only those are walked.
func computeAvailability(in availabilityInput) availability {
	n := len(in.complete)
	if n == 0 {
		// Nothing to fetch. Guards the division below: a NaN would make
		// json.Marshal fail the whole SSE frame.
		return availability{fraction: 1, known: true}
	}
	if in.seeders > 0 {
		return availability{fraction: 1, known: true}
	}
	settled := in.partialWebseeds == 0 && in.connected > 0 && in.peersFor >= availabilitySettle
	covered := n
	var a availability
	for it := scopeHoles(in.peers, in.offset, n).Iterator(); it.HasNext(); {
		i := int(it.Next()) - in.offset
		if in.complete[i] {
			continue
		}
		covered--
		if !settled {
			continue
		}
		if l := len(a.missing); l > 0 && a.missing[l-1].end == i {
			a.missing[l-1].end = i + 1
		} else {
			a.missing = append(a.missing, pieceRange{i, i + 1})
		}
		if i < len(in.wanted) && in.wanted[i] {
			a.wantedMissing++
		}
		if i < len(in.reading) && in.reading[i] {
			a.readerMissing++
		}
	}
	a.fraction = float32(float64(covered) / float64(n))
	// Nothing missing is exact whatever the peers have yet to announce:
	// announcements only add.
	a.known = settled || covered == n
	a.missing = capRanges(a.missing, maxMissingRanges)
	return a
}

// scopeHoles is the torrent indices of the scope [offset, offset+n) that none
// of the peer sets holds. The sets are only read: AndNot changes the scope
// bitmap, built here, and not its argument. A peer that sent HAVE_ALL before
// the torrent's info was known claims [0, 2^32); the operation touches only
// the scope's containers, so that costs nothing extra here. readSwarm would
// pay for it (cloning and folding such a set: ~5 ms and ~12 MB a peer) but
// never sees one: TorrentMap adds every torrent with its info.
func scopeHoles(peers []*roaring.Bitmap, offset, n int) *roaring.Bitmap {
	holes := roaring.New()
	holes.AddRange(uint64(offset), uint64(offset)+uint64(n))
	for _, p := range peers {
		if holes.IsEmpty() {
			break
		}
		holes.AndNot(p)
	}
	return holes
}

// capRanges merges ranges until at most limit remain, closing the smallest
// gaps first (on equal gaps, the earlier one). That paints the fewest
// pieces missing that are not; the gaps closed are pieces complete here or
// announced by a peer. rs is sorted and disjoint; it may be reused.
func capRanges(rs []pieceRange, limit int) []pieceRange {
	if len(rs) <= limit {
		return rs
	}
	gaps := make([]int, len(rs)-1) // gaps[i] lies between rs[i] and rs[i+1]
	for i := range gaps {
		gaps[i] = i
	}
	gap := func(i int) int { return rs[i+1].start - rs[i].end }
	slices.SortFunc(gaps, func(a, b int) int {
		return cmp.Or(cmp.Compare(gap(a), gap(b)), cmp.Compare(a, b))
	})
	merge := make([]bool, len(rs)-1)
	for _, g := range gaps[:len(rs)-limit] {
		merge[g] = true
	}
	out := rs[:1]
	for i := 1; i < len(rs); i++ {
		if merge[i-1] {
			out[len(out)-1].end = rs[i].end
		} else {
			out = append(out, rs[i])
		}
	}
	return out
}
