package services

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	logrusmiddleware "github.com/bakins/logrus-middleware"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc/metadata"

	pb "github.com/webtor-io/torrent-web-seeder/proto"
)

// statForUser is statFor as uid sees it: StatWeb passes X-Uid as user-id.
func statForUser(t *testing.T, st *Stat, h, uid, path string) *pb.StatReply {
	t.Helper()
	ctx := metadata.NewIncomingContext(context.Background(), metadata.MD{"info-hash": {h}, "user-id": {uid}})
	rep, err := st.Stat(ctx, &pb.StatRequest{Path: path})
	if err != nil {
		t.Fatalf("stat %q as %q: %v", path, uid, err)
	}
	return rep
}

// Each account sees its own torrent: its tracker's refusal and its own peer_id and
// port; a viewer without a loaded pair sees cold frames.
func TestStat_PerUser(t *testing.T) {
	env := newPrivateEnv(t, true)
	env.trk.Refuse(keyA)
	env.setRecords(testRecord{uidA, keyA}, testRecord{uidB, keyB})
	torA, _, _ := env.tm.Get(context.Background(), env.h, uidA)
	env.tm.Get(context.Background(), env.h, uidB)
	waitFor(t, "A's tracker to refuse", torA.AllAnnouncesFailing)
	env.trk.waitEvent(t, keyB, "started")
	st := NewStat(env.tm, env.dir)
	if r := statForUser(t, st, env.h, uidA, ""); !r.GetLive() || !r.GetTrackerRefused() {
		t.Errorf("A: live=%v refused=%v, want live and refused", r.GetLive(), r.GetTrackerRefused())
	}
	if r := statForUser(t, st, env.h, uidB, ""); !r.GetLive() || r.GetTrackerRefused() {
		t.Errorf("B: live=%v refused=%v, want live, not refused", r.GetLive(), r.GetTrackerRefused())
	}
	if r := statForUser(t, st, env.h, "", ""); r.GetLive() {
		t.Error("anonymous sees a live frame of an account's torrent")
	}
	a, b := env.trk.Announces(keyA)[0], env.trk.Announces(keyB)[0]
	if a.peerID == b.peerID || a.port == b.port {
		t.Errorf("A and B share peer_id %q/%q or port %d/%d", a.peerID, b.peerID, a.port, b.port)
	}

	// ?stats passes X-Uid on as user-id. The stream runs until the viewer
	// leaves, here the request's deadline.
	logger := log.New()
	logger.SetOutput(io.Discard)
	srv := (&logrusmiddleware.Middleware{Logger: logger}).Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = NewStatWeb(st).Serve(w, r, env.h, "")
	}), "")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/"+env.h+"/?stats", nil).WithContext(ctx)
	req.Header.Set("X-Uid", uidA)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), `"tracker_refused":true`) {
		t.Errorf("?stats with A's X-Uid: %q, want A's refusal", w.Body.String())
	}
}

// Each account's frame reads its own torrent's peers (Stat.swarms by the
// frame's key, not the infohash): B's torrent has a partial peer, A's none,
// and within one second neither frame borrows the other's, whichever asks
// first.
func TestStat_PerUserSwarm(t *testing.T) {
	data, mi := availabilityTestPayload(t)
	peerCl := swarmPeer(t, data, mi, 4*addTestPieceLen, 4)
	_, torB := swarmLeecher(t, mi, peerCl, 4)
	torA, err := addTorrent(mmapClient(t, t.TempDir(), false), mi)
	if err != nil {
		t.Fatal(err)
	}
	h := torA.InfoHash().HexString()
	tm := &TorrentMap{entries: map[string]*torrentEntry{
		entryKey(h, uidA): {t: torA, peers: &peerTimeline{}},
		entryKey(h, uidB): {t: torB, peers: timelineSince(time.Minute)},
	}}
	for _, order := range [][]string{{uidA, uidB}, {uidB, uidA}} {
		st := NewStat(tm, "")
		for _, uid := range order {
			r := statForUser(t, st, h, uid, "")
			got := fmt.Sprintf("peers=%d availability=%v known=%v missing=%v", r.GetPeers(), r.GetAvailability(), r.GetAvailabilityKnown(), rangesOf(r))
			want := "peers=0 availability=0 known=false missing=[]"
			if uid == uidB {
				want = fmt.Sprintf("peers=1 availability=%v known=true missing=[[4 9]]", float32(4.0/9))
			}
			if got != want {
				t.Errorf("order %s: %s's frame %s, want %s", acct(order[0])+","+acct(order[1]), acct(uid), got, want)
			}
		}
		if n := st.swarmReads.Load(); n != 2 {
			t.Errorf("two accounts' torrents read %d swarms, want 2", n)
		}
	}
}
