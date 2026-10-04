package services

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/webtor-io/torrent-web-seeder/proto"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden files in testdata")

const goldenStatStream = "testdata/statstream_availability.sse"

// The stats stream web-ui parses, byte for byte: SSE frames carrying the
// swarm availability fields, from real replies (a loaded torrent, loopback
// peers) sent the way StatStream sends them (nextFrame, StatStreamServer).
// The unit tests check the keys through a map and web-ui's tests parse
// hand-written JSON, so a renamed key, protojson's camelCase keys and string
// int64s, or a changed missing_unchanged would leave both green; this file
// is the contract they share. web-ui keeps a copy for its own test: after a
// deliberate change, rerun with -update, read the diff, copy the file over.
//
// Frames, in order (": " lines are SSE comments naming them):
//  0. root, peers connected under 20 s: availability_known false, missing
//     null, availability a lower bound;
//  1. root, settled: pieces 4..8 on nobody, piece 8 wanted;
//  2. the next frame after piece 0 completed: missing left out,
//     missing_unchanged true;
//  3. the next frame after a seeder connected: nothing missing, missing
//     null and missing_unchanged false;
//  4. a torrent the seeder has not loaded: live false, availability zero;
//  5. root, settled, a reader blocked on piece 8: reader_missing 1, the
//     frame web-ui turns into "missing" (taken before piece 8 arrives,
//     sent last so frames 0..4 stay as they were).
func TestStatStream_GoldenAvailability(t *testing.T) {
	data, mi := availabilityTestPayload(t)
	peerCl := swarmPeer(t, data, mi, 4*addTestPieceLen, 4)
	cl, tor := swarmLeecher(t, mi, peerCl, 4)
	h := tor.InfoHash().HexString()

	rec := httptest.NewRecorder()
	stream := func() *StatStreamServer { return NewStatStreamServer(context.Background(), rec, rec) }
	send := func(ss *StatStreamServer, note string, rep, prev *pb.StatReply) {
		t.Helper()
		f := nextFrame(rep, prev)
		if f == nil {
			t.Fatalf("%s: no frame", note)
		}
		fmt.Fprintf(rec, ": %s\n", note)
		if err := ss.Send(f); err != nil {
			t.Fatal(err)
		}
	}
	// A fresh Stat per reply: the cache would answer the previous one.
	live := func(tl *peerTimeline) *pb.StatReply {
		return statFor(t, NewStat(statMap(cl, h, tl), ""), h, "")
	}

	send(stream(), "root, peers connected under 20 s", live(timelineSince(0)), nil)

	ss := stream()
	first := live(timelineSince(time.Minute))
	send(ss, "root, settled", first, nil)
	stopRead := blockReaderOn(t, tor, 8)
	blocked := live(timelineSince(time.Minute))
	stopRead()
	tor.DownloadPieces(0, 1)
	waitFor(t, "piece 0 from the peer", func() bool { return tor.Piece(0).State().Complete })
	second := live(timelineSince(time.Minute))
	send(ss, "next frame: piece 0 completed, holes the same", second, first)
	seeder := swarmPeer(t, data, mi, len(data), 9)
	tor.AddClientPeer(seeder)
	waitFor(t, "piece 8 from a seeder", func() bool {
		s := tor.Stats()
		return tor.Piece(8).State().Complete && s.ActivePeers == 2 && s.ConnectedSeeders == 1
	})
	send(ss, "next frame: a seeder connected, nothing missing", live(nil), second)

	src := filepath.Join(t.TempDir(), "pack.torrent")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := mi.Write(f); err != nil {
		t.Fatal(err)
	}
	f.Close()
	cold := statMap(mmapClient(t, t.TempDir(), false), h, nil)
	cold.fsm = &FileStoreMap{p: src}
	send(stream(), "a torrent not loaded here", statFor(t, NewStat(cold, ""), h, ""), nil)
	send(stream(), "root, settled, a reader blocked on piece 8", blocked, nil)

	got := rec.Body.Bytes()
	if *updateGolden {
		if err := os.WriteFile(goldenStatStream, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(goldenStatStream)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("the stats stream differs from %s (a contract change: rerun with -update, then update web-ui's copy)\n--- got\n%s\n--- want\n%s", goldenStatStream, got, want)
	}
}
