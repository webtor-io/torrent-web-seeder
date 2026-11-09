package services

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"

	pb "github.com/webtor-io/torrent-web-seeder/proto"
)

const (
	trackerRefuses = "d14:failure reason9:bad ratioe"
	trackerAnswers = "d8:intervali1800e5:peers0:e"
)

// announcingTorrent is a torrent loaded in a real client that announces to one
// tracker, which answers every announce with body.
func announcingTorrent(t *testing.T, private bool, body string) (*Stat, *torrent.Torrent, *atomic.Int32) {
	t.Helper()
	var announces atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body)
		announces.Add(1)
	}))
	t.Cleanup(srv.Close)

	_, mi := availabilityTestPayload(t)
	if private {
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
	mi.Announce = srv.URL + "/announce"

	cfg := addTestConfig(t.TempDir())
	cfg.DisableTrackers = false
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })
	tor, err := cl.AddTorrent(mi)
	if err != nil {
		t.Fatal(err)
	}
	return NewStat(statMap(cl, tor.InfoHash().HexString(), timelineSince(time.Minute)), ""), tor, &announces
}

// Every scope of a private torrent says its tracker refused: the warm-up asks
// for a file, a directory download for a directory, the page for the root.
func TestStat_TrackerRefusedOnPrivateTorrent(t *testing.T) {
	st, tor, _ := announcingTorrent(t, true, trackerRefuses)
	waitFor(t, "the announce to fail", tor.AllAnnouncesFailing)
	h := tor.InfoHash().HexString()
	for _, path := range []string{"", "pack/a.bin", "pack/sub"} {
		if !statFor(t, st, h, path).GetTrackerRefused() {
			t.Errorf("path %q: tracker_refused false", path)
		}
	}
}

// A public torrent has DHT and PEX too: a failing tracker does not explain an
// empty swarm, so the reply does not claim it does.
func TestStat_TrackerRefusalIgnoredOnPublicTorrent(t *testing.T) {
	st, tor, _ := announcingTorrent(t, false, trackerRefuses)
	waitFor(t, "the announce to fail", tor.AllAnnouncesFailing)
	if statFor(t, st, tor.InfoHash().HexString(), "").GetTrackerRefused() {
		t.Error("tracker_refused true on a public torrent")
	}
}

func TestStat_AnsweringTrackerIsNotARefusal(t *testing.T) {
	st, tor, announces := announcingTorrent(t, true, trackerAnswers)
	waitFor(t, "an announce", func() bool { return announces.Load() > 0 })
	// The client stores the result after the response has been read.
	time.Sleep(200 * time.Millisecond)
	if statFor(t, st, tor.InfoHash().HexString(), "").GetTrackerRefused() {
		t.Error("tracker_refused true while the tracker answers")
	}
}

// A torrent with no peers changes nothing else, and StatStream sends only what
// changed: the flag alone has to produce a frame, and the frame has to carry it.
func TestNextFrame_CarriesTrackerRefused(t *testing.T) {
	prev := &pb.StatReply{Total: 100, Live: true}
	rep := &pb.StatReply{Total: 100, Live: true, TrackerRefused: true}
	f := nextFrame(rep, prev)
	if f == nil {
		t.Fatal("no frame for a change of tracker_refused")
	}
	if !f.GetTrackerRefused() {
		t.Error("frame lost tracker_refused")
	}
}
