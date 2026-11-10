package services

import (
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestPeerSourceLabel(t *testing.T) {
	for src, want := range map[torrent.PeerSource]string{
		torrent.PeerSourceTracker:         "tracker",
		torrent.PeerSourceDhtGetPeers:     "dht",
		torrent.PeerSourceDhtAnnouncePeer: "dht",
		torrent.PeerSourcePex:             "pex",
		torrent.PeerSourceIncoming:        "incoming",
		torrent.PeerSourceDirect:          "other",
		torrent.PeerSourceUtHolepunch:     "other",
		"":                                "other", // web seeds, AddClientPeer
	} {
		if got := peerSourceLabel(src); got != want {
			t.Errorf("%q: %q, want %q", src, got, want)
		}
	}
}

// makePrivate sets the BEP 27 private flag in mi's info.
func makePrivate(t *testing.T, mi *metainfo.MetaInfo) {
	t.Helper()
	info, err := mi.UnmarshalInfo()
	if err != nil {
		t.Fatal(err)
	}
	yes := true
	info.Private = &yes
	if mi.InfoBytes, err = bencode.Marshal(info); err != nil {
		t.Fatal(err)
	}
}

// privatePeerCounts reads both counters for every source label.
func privatePeerCounts() (bytes, conns map[string]float64) {
	bytes, conns = map[string]float64{}, map[string]float64{}
	for _, l := range peerSourceLabels {
		bytes[l] = testutil.ToFloat64(promPrivatePeerUsefulBytes.WithLabelValues(l))
		conns[l] = testutil.ToFloat64(promPrivatePeerConns.WithLabelValues(l))
	}
	return
}

func countsSince(before, after map[string]float64) map[string]float64 {
	d := map[string]float64{}
	for l, v := range after {
		if v != before[l] {
			d[l] = v - before[l]
		}
	}
	return d
}

// A torrent downloaded by the client the service builds, from one peer over
// loopback. The private torrent's useful bytes and connection land under the
// peer's source, and the bytes are exactly the client's useful bytes; a public
// torrent's land nowhere.
func TestTorrentClient_PrivatePeerSources(t *testing.T) {
	for _, c := range []struct {
		name     string
		private  bool
		incoming bool // the peer dials us, instead of being added with source pex
		want     string
	}{
		{"private from pex", true, false, "pex"},
		{"private incoming", true, true, "incoming"},
		{"public", false, false, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			swarm := newSwarmStats()
			tc := &TorrentClient{
				swarm:              swarm,
				testConfig:         loopbackConfig,
				rLimit:             -1,
				maxUnverifiedBytes: -1,
				dataDir:            t.TempDir(),
			}
			t.Cleanup(tc.Close)
			cl, err := tc.Get()
			if err != nil {
				t.Fatal(err)
			}
			data, mi := addTestPayload(t)
			if c.private {
				makePrivate(t, mi)
			}
			seeder := seedingClient(t, data, mi)
			b0, c0 := privatePeerCounts()
			tor, err := addTorrent(cl, mi)
			if err != nil {
				t.Fatal(err)
			}
			if c.incoming {
				st, ok := seeder.Torrent(mi.HashInfoBytes())
				if !ok {
					t.Fatal("the seeder lost its torrent")
				}
				st.AddClientPeer(cl)
			} else {
				var peers []torrent.PeerInfo
				for _, la := range seeder.ListenAddrs() {
					peers = append(peers, torrent.PeerInfo{Addr: la, Source: torrent.PeerSourcePex, Trusted: true})
				}
				tor.AddPeers(peers)
			}
			readAllWithin(t, tor, data, 20*time.Second)
			// Let a connection still closing settle; nothing more is counted.
			time.Sleep(50 * time.Millisecond)

			b1, c1 := privatePeerCounts()
			db, dc := countsSince(b0, b1), countsSince(c0, c1)
			if c.want == "" {
				if len(db) != 0 || len(dc) != 0 {
					t.Errorf("a public torrent was counted: bytes %v, connections %v", db, dc)
				}
				return
			}
			useful := float64(swarm.totals().readUseful)
			if useful < float64(len(data)) {
				t.Fatalf("the client counted %v useful bytes of %d", useful, len(data))
			}
			if len(db) != 1 || db[c.want] != useful {
				t.Errorf("useful bytes by source %v, want %v under %q only", db, useful, c.want)
			}
			if len(dc) != 1 || dc[c.want] < 1 {
				t.Errorf("connections by source %v, want under %q only", dc, c.want)
			}
		})
	}
}
