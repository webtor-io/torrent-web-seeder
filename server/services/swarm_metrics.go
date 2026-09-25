package services

import (
	"sync"

	"github.com/anacrolix/torrent"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	promPeerReadBytesDesc = prometheus.NewDesc(
		"torrent_web_seeder_peer_read_bytes_total",
		"Bytes received from peers. wire: everything on the connection, handshakes and encryption included; "+
			"data: piece payload, duplicates and unwanted chunks included; useful: piece payload that was needed.",
		[]string{"kind"}, nil)
	promPeerWrittenBytesDesc = prometheus.NewDesc(
		"torrent_web_seeder_peer_written_bytes_total",
		"Bytes sent to peers. wire: everything on the connection; data: piece payload.",
		[]string{"kind"}, nil)
	promPiecesVerifiedDesc = prometheus.NewDesc(
		"torrent_web_seeder_pieces_verified_total",
		"Pieces written with data from peers and then hash-checked. good: passed; "+
			"bad: failed with every chunk from peers and no storage error.",
		[]string{"result"}, nil)
)

// promSwarm is the collector the service registers; every TorrentClient
// built by NewTorrentClient reports its clients to it.
var promSwarm = newSwarmStats()

// connStatsSource is a torrent client as swarmStats reads it.
type connStatsSource interface {
	ConnStats() torrent.ConnStats
}

// swarmTotals holds the exported torrent.ConnStats fields as plain numbers.
type swarmTotals struct {
	readWire, readData, readUseful int64
	writtenWire, writtenData       int64
	piecesGood, piecesBad          int64
}

func totalsOf(cs *torrent.ConnStats) swarmTotals {
	return swarmTotals{
		readWire:    cs.BytesRead.Int64(),
		readData:    cs.BytesReadData.Int64(),
		readUseful:  cs.BytesReadUsefulData.Int64(),
		writtenWire: cs.BytesWritten.Int64(),
		writtenData: cs.BytesWrittenData.Int64(),
		piecesGood:  cs.PiecesDirtiedGood.Int64(),
		piecesBad:   cs.PiecesDirtiedBad.Int64(),
	}
}

func (a swarmTotals) plus(b swarmTotals) swarmTotals {
	return swarmTotals{
		readWire:    a.readWire + b.readWire,
		readData:    a.readData + b.readData,
		readUseful:  a.readUseful + b.readUseful,
		writtenWire: a.writtenWire + b.writtenWire,
		writtenData: a.writtenData + b.writtenData,
		piecesGood:  a.piecesGood + b.piecesGood,
		piecesBad:   a.piecesBad + b.piecesBad,
	}
}

// swarmStats exports what the torrent clients exchanged with peers: bytes each
// way and pieces that passed or failed their hash. It is a collector, so a
// scrape reads the clients' counters as they are, with no ticker. The library
// keeps them as atomics in the client (Client.ConnStats copies them without
// taking the client lock), so a scrape never waits on a busy client, and a
// client's counters stay readable during and after its Close.
//
// The service closes a client that has been idle for clientIdleTimeout and
// builds a new one on the next request, and the new one counts from zero.
// retire folds the closed client's final counts into closed, so the exported
// counters only grow for the life of the process, and forgets the client, which
// a collector must not keep in memory.
type swarmStats struct {
	mu     sync.Mutex
	live   map[connStatsSource]struct{}
	closed swarmTotals
}

func newSwarmStats() *swarmStats {
	return &swarmStats{live: map[connStatsSource]struct{}{}}
}

// attach starts counting a new client.
func (s *swarmStats) attach(c connStatsSource) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.live[c] = struct{}{}
}

// retire is called after c is closed, so that what it counted while closing is
// kept. A scrape reads the live clients under the same lock, so it sees c
// either live or folded in, never neither: the counters never go down.
func (s *swarmStats) retire(c connStatsSource) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.live[c]; !ok {
		// Retired already: its counts are in closed.
		return
	}
	delete(s.live, c)
	cs := c.ConnStats()
	s.closed = s.closed.plus(totalsOf(&cs))
}

func (s *swarmStats) totals() swarmTotals {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.closed
	for c := range s.live {
		cs := c.ConnStats()
		t = t.plus(totalsOf(&cs))
	}
	return t
}

func (s *swarmStats) Describe(ch chan<- *prometheus.Desc) {
	ch <- promPeerReadBytesDesc
	ch <- promPeerWrittenBytesDesc
	ch <- promPiecesVerifiedDesc
}

// Collect exports every label value, at zero before any client exists.
func (s *swarmStats) Collect(ch chan<- prometheus.Metric) {
	t := s.totals()
	counter := func(d *prometheus.Desc, v int64, label string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, float64(v), label)
	}
	counter(promPeerReadBytesDesc, t.readWire, "wire")
	counter(promPeerReadBytesDesc, t.readData, "data")
	counter(promPeerReadBytesDesc, t.readUseful, "useful")
	counter(promPeerWrittenBytesDesc, t.writtenWire, "wire")
	counter(promPeerWrittenBytesDesc, t.writtenData, "data")
	counter(promPiecesVerifiedDesc, t.piecesGood, "good")
	counter(promPiecesVerifiedDesc, t.piecesBad, "bad")
}
