package services

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

var swarmMetricNames = []string{
	"torrent_web_seeder_peer_read_bytes_total",
	"torrent_web_seeder_peer_written_bytes_total",
	"torrent_web_seeder_pieces_verified_total",
}

// swarmSeries scrapes c the way the service's registry does and returns
// `name{label=value}` -> value.
func swarmSeries(t *testing.T, c prometheus.Collector) map[string]float64 {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatal(err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			var labels []string
			for _, l := range m.GetLabel() {
				labels = append(labels, l.GetName()+"="+l.GetValue())
			}
			sort.Strings(labels)
			got[mf.GetName()+"{"+strings.Join(labels, ",")+"}"] = m.GetCounter().GetValue()
		}
	}
	return got
}

func assertSeries(t *testing.T, got, want map[string]float64) {
	t.Helper()
	for k, v := range want {
		if g, ok := got[k]; !ok {
			t.Errorf("%s missing", k)
		} else if g != v {
			t.Errorf("%s = %v, want %v", k, g, v)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected series %s", k)
		}
	}
}

// fakeConnStats is a client whose counters the test sets.
type fakeConnStats struct{ cs torrent.ConnStats }

func (f *fakeConnStats) ConnStats() torrent.ConnStats { return f.cs.Copy() }

func (f *fakeConnStats) addEach(n int64) {
	for _, c := range []*torrent.Count{
		&f.cs.BytesRead, &f.cs.BytesReadData, &f.cs.BytesReadUsefulData,
		&f.cs.BytesWritten, &f.cs.BytesWrittenData,
		&f.cs.PiecesDirtiedGood, &f.cs.PiecesDirtiedBad,
	} {
		c.Add(n)
	}
}

func (a swarmTotals) atLeast(b swarmTotals) bool {
	return a.readWire >= b.readWire && a.readData >= b.readData && a.readUseful >= b.readUseful &&
		a.writtenWire >= b.writtenWire && a.writtenData >= b.writtenData &&
		a.piecesGood >= b.piecesGood && a.piecesBad >= b.piecesBad
}

// Every label value is exported from the first scrape, before the service has
// built a client (it builds one on the first request), so a rate() over a
// fresh pod starts at zero instead of at the first value seen.
func TestSwarmStats_EveryLabelAtZeroBeforeAnyClient(t *testing.T) {
	assertSeries(t, swarmSeries(t, newSwarmStats()), map[string]float64{
		"torrent_web_seeder_peer_read_bytes_total{kind=wire}":    0,
		"torrent_web_seeder_peer_read_bytes_total{kind=data}":    0,
		"torrent_web_seeder_peer_read_bytes_total{kind=useful}":  0,
		"torrent_web_seeder_peer_written_bytes_total{kind=wire}": 0,
		"torrent_web_seeder_peer_written_bytes_total{kind=data}": 0,
		"torrent_web_seeder_pieces_verified_total{result=good}":  0,
		"torrent_web_seeder_pieces_verified_total{result=bad}":   0,
	})

	// The service's collector is in the registry /metrics serves.
	n, err := testutil.GatherAndCount(prometheus.DefaultGatherer, swarmMetricNames...)
	if err != nil {
		t.Fatal(err)
	}
	if n != 7 {
		t.Errorf("the default registry has %d swarm series, want 7", n)
	}
}

// Each label reads its own ConnStats field, and the fields the metrics do not
// export stay out.
func TestSwarmStats_ExportsConnStatsFields(t *testing.T) {
	f := &fakeConnStats{}
	f.cs.BytesRead.Add(1100)
	f.cs.BytesReadData.Add(1000)
	f.cs.BytesReadUsefulData.Add(900)
	f.cs.BytesReadUsefulIntendedData.Add(7)
	f.cs.BytesWritten.Add(600)
	f.cs.BytesWrittenData.Add(500)
	f.cs.ChunksRead.Add(7)
	f.cs.PiecesDirtiedGood.Add(30)
	f.cs.PiecesDirtiedBad.Add(2)
	s := newSwarmStats()
	s.attach(f)
	assertSeries(t, swarmSeries(t, s), map[string]float64{
		"torrent_web_seeder_peer_read_bytes_total{kind=wire}":    1100,
		"torrent_web_seeder_peer_read_bytes_total{kind=data}":    1000,
		"torrent_web_seeder_peer_read_bytes_total{kind=useful}":  900,
		"torrent_web_seeder_peer_written_bytes_total{kind=wire}": 600,
		"torrent_web_seeder_peer_written_bytes_total{kind=data}": 500,
		"torrent_web_seeder_pieces_verified_total{result=good}":  30,
		"torrent_web_seeder_pieces_verified_total{result=bad}":   2,
	})
}

// A closed client's counts stay in the totals after the client is gone, and
// the next client's counts, from zero, add on top: the counters never go down.
func TestSwarmStats_KeepsClosedClientCounts(t *testing.T) {
	s := newSwarmStats()
	a := &fakeConnStats{}
	s.attach(a)
	a.addEach(100)
	before := s.totals()

	s.retire(a)
	if len(s.live) != 0 {
		t.Fatalf("%d clients still held after retire", len(s.live))
	}
	if got := s.totals(); got != before {
		t.Fatalf("totals %+v after the client closed, %+v before", got, before)
	}

	b := &fakeConnStats{}
	s.attach(b)
	if got := s.totals(); got != before {
		t.Fatalf("totals %+v with a fresh client, %+v before", got, before)
	}
	b.addEach(5)
	want := before.plus(swarmTotals{5, 5, 5, 5, 5, 5, 5})
	if got := s.totals(); got != want {
		t.Fatalf("totals %+v, want %+v", got, want)
	}
}

// Retiring a client twice (the idle close and the shutdown Close racing on
// it) counts it once.
func TestSwarmStats_RetireTwiceCountsOnce(t *testing.T) {
	s := newSwarmStats()
	a := &fakeConnStats{}
	s.attach(a)
	a.addEach(100)
	s.retire(a)
	want := s.totals()
	s.retire(a)
	if got := s.totals(); got != want {
		t.Fatalf("totals %+v after a second retire, want %+v", got, want)
	}
}

// Real clients exchanging a torrent over loopback: the downloader counts what
// it received and verified, the seeding peer what it sent.
func TestSwarmStats_CountsRealTransfer(t *testing.T) {
	data, mi := addTestPayload(t)
	seeder := seedingClient(t, data, mi)
	up := newSwarmStats()
	up.attach(seeder)
	cl := mmapClient(t, t.TempDir(), false)
	down := newSwarmStats()
	down.attach(cl)

	tor, err := addTorrent(cl, mi)
	if err != nil {
		t.Fatal(err)
	}
	tor.AddClientPeer(seeder)
	readAllWithin(t, tor, data, 20*time.Second)

	n := int64(len(data))
	d, u := down.totals(), up.totals()
	t.Logf("downloader %+v, seeder %+v", d, u)
	if d.readUseful < n || d.readData < d.readUseful || d.readWire <= d.readData {
		t.Errorf("downloader read wire %d, data %d, useful %d for a %d-byte payload",
			d.readWire, d.readData, d.readUseful, n)
	}
	if d.piecesGood != int64(tor.NumPieces()) || d.piecesBad != 0 {
		t.Errorf("downloader verified %d good and %d bad pieces of %d",
			d.piecesGood, d.piecesBad, tor.NumPieces())
	}
	if u.writtenData < n || u.writtenWire <= u.writtenData {
		t.Errorf("seeder wrote wire %d, data %d for a %d-byte payload", u.writtenWire, u.writtenData, n)
	}
}

// loopbackConfig confines a client built by TorrentClient.get to loopback, as
// addTestConfig does: no DHT, trackers or port mapping.
func loopbackConfig(cfg *torrent.ClientConfig) {
	lo := addTestConfig("")
	cfg.ListenHost = lo.ListenHost
	cfg.ListenPort = lo.ListenPort
	cfg.NoDHT = lo.NoDHT
	cfg.DisableTrackers = lo.DisableTrackers
	cfg.NoDefaultPortForwarding = lo.NoDefaultPortForwarding
	cfg.DisableAcceptRateLimiting = lo.DisableAcceptRateLimiting
	cfg.DisableUTP = lo.DisableUTP
	cfg.DisableIPv6 = lo.DisableIPv6
	cfg.DisableWebtorrent = lo.DisableWebtorrent
	cfg.DisableWebseeds = lo.DisableWebseeds
	cfg.KeepAliveTimeout = lo.KeepAliveTimeout
}

// The service closes an idle client and builds a new one on the next request.
// The exported counters carry the closed client's counts across that, the
// collector lets go of the closed client, and the new client's traffic adds on
// top.
func TestTorrentClient_SwarmCountsSurviveIdleClose(t *testing.T) {
	swarm := newSwarmStats()
	tc := &TorrentClient{
		swarm:              swarm,
		testConfig:         loopbackConfig,
		rLimit:             -1,
		maxUnverifiedBytes: -1,
		dataDir:            t.TempDir(),
	}
	shutDown := false
	t.Cleanup(func() {
		if !shutDown {
			tc.Close()
		}
	})

	download := func(cl *torrent.Client) (n int64, pieces int) {
		t.Helper()
		data, mi := addTestPayload(t)
		seeder := seedingClient(t, data, mi)
		tor, err := addTorrent(cl, mi)
		if err != nil {
			t.Fatal(err)
		}
		tor.AddClientPeer(seeder)
		readAllWithin(t, tor, data, 20*time.Second)
		return int64(len(data)), tor.NumPieces()
	}

	cl1, err := tc.Get()
	if err != nil {
		t.Fatal(err)
	}
	n1, _ := download(cl1)
	first := swarm.totals()
	if first.readUseful < n1 {
		t.Fatalf("the client's download is not counted: %+v", first)
	}

	tc.closeIdle(cl1)
	if len(swarm.live) != 0 {
		t.Fatalf("the collector still holds %d clients after the idle close", len(swarm.live))
	}
	closed := swarm.totals()
	if !closed.atLeast(first) {
		t.Fatalf("counters went down when the client closed: %+v, %+v before", closed, first)
	}

	cl2, err := tc.Get()
	if err != nil {
		t.Fatal(err)
	}
	if cl2 == cl1 {
		t.Fatal("Get returned the closed client")
	}
	if fresh := swarm.totals(); !fresh.atLeast(closed) {
		t.Fatalf("counters went down with a new client: %+v, %+v before", fresh, closed)
	}
	n2, pieces2 := download(cl2)
	second := swarm.totals()
	if second.readUseful < closed.readUseful+n2 || second.piecesGood != closed.piecesGood+int64(pieces2) {
		t.Fatalf("the new client's download is not added on top: %+v, %+v before, %d bytes and %d pieces",
			second, closed, n2, pieces2)
	}

	tc.Close()
	shutDown = true
	if len(swarm.live) != 0 {
		t.Fatalf("the collector still holds %d clients after Close", len(swarm.live))
	}
	if final := swarm.totals(); !final.atLeast(second) {
		t.Fatalf("counters went down at Close: %+v, %+v before", final, second)
	}
}
