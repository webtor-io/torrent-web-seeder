package services

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"google.golang.org/grpc/metadata"

	pb "github.com/webtor-io/torrent-web-seeder/proto"
)

// sseFrames parses the statupdate frames out of an SSE body, each into a
// generic map so the test sees the JSON keys web-ui sees.
func sseFrames(t *testing.T, body string) []map[string]any {
	t.Helper()
	var frames []map[string]any
	event := ""
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: ") && event == "statupdate":
			var m map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m); err != nil {
				t.Fatalf("frame is not JSON: %v: %q", err, line)
			}
			frames = append(frames, m)
		}
	}
	return frames
}

// The SSE frame web-ui reads is json.Marshal of the StatStream frame. The
// swarm fields must be in it under these names, including their zero values
// (the generated code has omitempty stripped, `make protoc`): a reader told
// availability_known=false must not have to guess from a missing key.
func TestStatFrame_SSECarriesAvailability(t *testing.T) {
	rep := &pb.StatReply{
		Total: 100, Completed: 10, Peers: 3, Leechers: 3, Live: true,
		Status:            pb.StatReply_SEEDING,
		Availability:      0.75,
		AvailabilityKnown: true,
		Missing:           []*pb.PieceRange{{Start: 3, End: 5}, {Start: 7, End: 8}},
		WantedMissing:     2,
		ReaderMissing:     1,
	}
	rec := httptest.NewRecorder()
	ss := NewStatStreamServer(context.Background(), rec, rec)
	if err := ss.Send(statFrame(rep, nil, rep.GetPieces())); err != nil {
		t.Fatal(err)
	}
	if err := ss.Send(statFrame(&pb.StatReply{Live: false}, nil, nil)); err != nil {
		t.Fatal(err)
	}
	frames := sseFrames(t, rec.Body.String())
	if len(frames) != 2 {
		t.Fatalf("%d frames, want 2", len(frames))
	}
	f := frames[0]
	if f["availability"] != 0.75 || f["availability_known"] != true || f["wanted_missing"] != float64(2) ||
		f["reader_missing"] != float64(1) || f["missing_unchanged"] != false {
		t.Errorf("availability=%v availability_known=%v wanted_missing=%v reader_missing=%v missing_unchanged=%v, want 0.75/true/2/1/false",
			f["availability"], f["availability_known"], f["wanted_missing"], f["reader_missing"], f["missing_unchanged"])
	}
	missing, _ := json.Marshal(f["missing"])
	if string(missing) != `[{"end":5,"start":3},{"end":8,"start":7}]` {
		t.Errorf("missing=%s, want the two ranges as {start,end}", missing)
	}
	for _, k := range []string{"availability", "availability_known", "missing", "wanted_missing", "reader_missing", "missing_unchanged"} {
		if _, ok := frames[1][k]; !ok {
			t.Errorf("a cold frame lacks %q; zero values must be sent", k)
		}
	}
	if frames[1]["availability_known"] != false {
		t.Errorf("cold frame availability_known=%v, want false", frames[1]["availability_known"])
	}
}

// missing goes out only in frames where it changed: a frame sent because a
// piece completed would otherwise repeat up to 512 ranges. missing_unchanged
// tells "the same as before" from "now empty".
func TestStatFrame_MissingOnlyWhenChanged(t *testing.T) {
	prev := &pb.StatReply{Completed: 10, Missing: []*pb.PieceRange{{Start: 3, End: 5}}}
	for _, c := range []struct {
		name          string
		rep, prev     *pb.StatReply
		wantMissing   int
		wantUnchanged bool
	}{
		{"first frame", prev, nil, 1, false},
		{"pieces completed, holes the same", &pb.StatReply{Completed: 20, Missing: []*pb.PieceRange{{Start: 3, End: 5}}}, prev, 0, true},
		{"holes moved", &pb.StatReply{Completed: 20, Missing: []*pb.PieceRange{{Start: 4, End: 5}}}, prev, 1, false},
		{"holes gone", &pb.StatReply{Completed: 20}, prev, 0, false},
		{"still no holes", &pb.StatReply{Completed: 30}, &pb.StatReply{Completed: 20}, 0, true},
	} {
		f := statFrame(c.rep, c.prev, nil)
		if len(f.GetMissing()) != c.wantMissing || f.GetMissingUnchanged() != c.wantUnchanged {
			t.Errorf("%s: %d ranges, missing_unchanged=%v; want %d/%v", c.name, len(f.GetMissing()), f.GetMissingUnchanged(), c.wantMissing, c.wantUnchanged)
		}
	}
}

// StatStream sends a frame only when something a client draws changed. A
// torrent stuck with peers changes nothing else: no bytes, no piece states,
// often the same peer count — only which pieces the peers have.
func TestStatChanged_Availability(t *testing.T) {
	base := func() *pb.StatReply {
		return &pb.StatReply{
			Total: 100, Completed: 10, Peers: 3, Leechers: 3, Live: true,
			Availability: 0.5, AvailabilityKnown: true,
			Missing:       []*pb.PieceRange{{Start: 3, End: 5}},
			WantedMissing: 1,
			ReaderMissing: 1,
		}
	}
	if statChanged(base(), base(), nil) {
		t.Fatal("identical replies reported as changed")
	}
	for name, mut := range map[string]func(r *pb.StatReply){
		"availability":       func(r *pb.StatReply) { r.Availability = 0.6 },
		"availability_known": func(r *pb.StatReply) { r.AvailabilityKnown = false },
		"wanted_missing":     func(r *pb.StatReply) { r.WantedMissing = 0 },
		"reader_missing":     func(r *pb.StatReply) { r.ReaderMissing = 0 },
		"missing end":        func(r *pb.StatReply) { r.Missing[0].End = 6 },
		"missing start":      func(r *pb.StatReply) { r.Missing[0].Start = 2 },
		"missing added":      func(r *pb.StatReply) { r.Missing = append(r.Missing, &pb.PieceRange{Start: 8, End: 9}) },
		"missing cleared":    func(r *pb.StatReply) { r.Missing = nil },
	} {
		cur := base()
		mut(cur)
		if !statChanged(cur, base(), nil) {
			t.Errorf("a change in %s alone sends no frame", name)
		}
	}
	if !statChanged(base(), nil, nil) {
		t.Error("the first reply must be sent")
	}
}

// Two files over nine 16 KiB pieces, the second in a directory:
//
//	pack/a.bin      pieces 0..3 (three pieces and 1000 B of piece 3)
//	pack/sub/b.bin  pieces 3..8 (from 1000 B into piece 3)
//
// so a file's and a directory's first piece is not the torrent's.
func availabilityTestPayload(t *testing.T) ([]byte, *metainfo.MetaInfo) {
	t.Helper()
	const pl = addTestPieceLen
	aLen, bLen := int64(3*pl+1000), int64(5*pl+500)
	data := make([]byte, aLen+bLen)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	var pieces []byte
	for off := 0; off < len(data); off += pl {
		h := sha1.Sum(data[off:min(off+pl, len(data))])
		pieces = append(pieces, h[:]...)
	}
	infoBytes, err := bencode.Marshal(metainfo.Info{
		Name:        "pack",
		PieceLength: pl,
		Pieces:      pieces,
		Files: []metainfo.FileInfo{
			{Path: []string{"a.bin"}, Length: aLen},
			{Path: []string{"sub", "b.bin"}, Length: bLen},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return data, &metainfo.MetaInfo{InfoBytes: infoBytes}
}

// swarmPeer is a client with the payload's first `have` bytes (zeros after,
// which fail the hash) in the library's file storage, checked: it claims
// exactly the pieces inside those bytes.
func swarmPeer(t *testing.T, data []byte, mi *metainfo.MetaInfo, have int, wantPieces int) *torrent.Client {
	t.Helper()
	dir := t.TempDir()
	partial := make([]byte, len(data))
	copy(partial, data[:have])
	info, err := mi.UnmarshalInfo()
	if err != nil {
		t.Fatal(err)
	}
	off := int64(0)
	for _, fi := range info.Files {
		p := filepath.Join(append([]string{dir, info.Name}, fi.Path...)...)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, partial[off:off+fi.Length], 0o644); err != nil {
			t.Fatal(err)
		}
		off += fi.Length
	}
	cfg := addTestConfig(dir)
	cfg.Seed = true
	st := storage.NewFileOpts(storage.NewFileClientOpts{
		ClientBaseDir:   dir,
		PieceCompletion: storage.NewMapPieceCompletion(),
		UsePartFiles:    g.Some(false),
	})
	cfg.DefaultStorage = st
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cl.Close()
		_ = st.Close()
	})
	tor, err := cl.AddTorrent(mi)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		busy, complete := false, 0
		for _, r := range tor.PieceStateRuns() {
			busy = busy || r.Checking || r.Hashing || r.QueuedForHash || r.Marking || !r.Ok
			if r.Complete {
				complete += r.Length
			}
		}
		if !busy {
			if complete != wantPieces {
				t.Fatalf("peer checked %d complete pieces, want %d", complete, wantPieces)
			}
			return cl
		}
		if time.Now().After(deadline) {
			t.Fatalf("peer still checking after 10 s: %v", tor.PieceStateRuns())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// swarmLeecher is this service's client holding the torrent, connected to
// peer; it returns once the peer's piece set (wantPieces claimed) has
// arrived. It wants the last piece, 8: the library dials only for a torrent
// that needs data, and the partial peers do not have that piece, so the
// connection stays up without a download.
func swarmLeecher(t *testing.T, mi *metainfo.MetaInfo, peer *torrent.Client, wantPieces int) (*torrent.Client, *torrent.Torrent) {
	t.Helper()
	return swarmLeecherOn(t, mmapClient(t, t.TempDir(), false), mi, peer, wantPieces)
}

// swarmLeecherOn is swarmLeecher on a given client.
func swarmLeecherOn(t *testing.T, cl *torrent.Client, mi *metainfo.MetaInfo, peer *torrent.Client, wantPieces int) (*torrent.Client, *torrent.Torrent) {
	t.Helper()
	tor, err := addTorrent(cl, mi)
	if err != nil {
		t.Fatal(err)
	}
	tor.DownloadPieces(8, 9)
	tor.AddClientPeer(peer)
	waitForPeerPieces(t, tor, wantPieces)
	return cl, tor
}

// waitForPeerPieces waits until tor has exactly one connection, claiming
// wantPieces pieces.
func waitForPeerPieces(t *testing.T, tor *torrent.Torrent, wantPieces int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		conns := tor.PeerConns()
		if len(conns) == 1 && int(conns[0].PeerPieces().GetCardinality()) == wantPieces {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("peer's pieces never arrived: %d conns", len(conns))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// statMap is a TorrentMap over cl that knows h with timeline tl (nil: the
// map does not track it).
func statMap(cl *torrent.Client, h string, tl *peerTimeline) *TorrentMap {
	m := &TorrentMap{tc: &TorrentClient{cl: cl, inited: true}, entries: map[string]*torrentEntry{}}
	if tl != nil {
		m.entries[h] = &torrentEntry{peers: tl}
	}
	return m
}

func timelineSince(d time.Duration) *peerTimeline {
	tl := &peerTimeline{}
	tl.observe(1, time.Now().Add(-d))
	return tl
}

func statFor(t *testing.T, st *Stat, h, path string) *pb.StatReply {
	t.Helper()
	ctx := metadata.NewIncomingContext(context.Background(), metadata.MD{"info-hash": []string{h}})
	rep, err := st.Stat(ctx, &pb.StatRequest{Path: path})
	if err != nil {
		t.Fatalf("stat %q: %v", path, err)
	}
	return rep
}

func rangesOf(rep *pb.StatReply) [][2]int64 {
	var out [][2]int64
	for _, r := range rep.GetMissing() {
		out = append(out, [2]int64{r.GetStart(), r.GetEnd()})
	}
	return out
}

type availabilityCase struct {
	path          string
	avail         float32
	missing       [][2]int64
	wantedMissing int32
	readerMissing int32
}

func checkAvailability(t *testing.T, st *Stat, h string, cases []availabilityCase) {
	t.Helper()
	for _, c := range cases {
		rep := statFor(t, st, h, c.path)
		if rep.GetAvailability() != c.avail || !rep.GetAvailabilityKnown() || !reflect.DeepEqual(rangesOf(rep), c.missing) ||
			rep.GetWantedMissing() != c.wantedMissing || rep.GetReaderMissing() != c.readerMissing {
			t.Errorf("%q: availability=%v known=%v missing=%v wanted_missing=%d reader_missing=%d, want %v/true/%v/%d/%d",
				c.path, rep.GetAvailability(), rep.GetAvailabilityKnown(), rangesOf(rep), rep.GetWantedMissing(), rep.GetReaderMissing(),
				c.avail, c.missing, c.wantedMissing, c.readerMissing)
		}
	}
}

// One peer with pieces 0..3, nothing here. Every scope reports against its
// own positions: the torrent's holes are 4..8, the file that starts inside
// piece 3 has its holes from its second position on.
func TestStat_AvailabilityFromPartialPeer(t *testing.T) {
	data, mi := availabilityTestPayload(t)
	peerCl := swarmPeer(t, data, mi, 4*addTestPieceLen, 4)
	cl, tor := swarmLeecher(t, mi, peerCl, 4)
	h := tor.InfoHash().HexString()
	st := NewStat(statMap(cl, h, timelineSince(time.Minute)), "")

	// Piece 8 is wanted (swarmLeecher) and on nobody; no reader is open.
	for _, rep := range []*pb.StatReply{statFor(t, st, h, "")} {
		if !rep.GetLive() || rep.GetPeers() != 1 || rep.GetSeeders() != 0 {
			t.Fatalf("live=%v peers=%d seeders=%d, want a live reply with one partial peer", rep.GetLive(), rep.GetPeers(), rep.GetSeeders())
		}
	}
	checkAvailability(t, st, h, []availabilityCase{
		{"", 4.0 / 9, [][2]int64{{4, 9}}, 1, 0},
		{"pack/a.bin", 1, nil, 0, 0},
		{"pack/sub/b.bin", 1.0 / 6, [][2]int64{{1, 6}}, 1, 0},
		{"pack/sub", 1.0 / 6, [][2]int64{{1, 6}}, 1, 0},
	})
	// Four paths of one torrent inside a second: one read of its peers.
	if n := st.swarmReads.Load(); n != 1 {
		t.Errorf("four scopes of one torrent read its swarm %d times, want 1", n)
	}

	// A stream blocked reading piece 8: a reader has priorities only while
	// in Read (reader.piecesUncached), and this one cannot finish — nobody
	// has the piece.
	r := tor.NewReader()
	readCtx, stopRead := context.WithCancel(context.Background())
	r.SetContext(readCtx)
	if _, err := r.Seek(8*addTestPieceLen, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		_, _ = r.Read(make([]byte, 1))
	}()
	waitFor(t, "the reader to wait on piece 8", func() bool {
		return tor.Piece(8).State().Priority >= torrent.PiecePriorityReadahead
	})
	reading := NewStat(statMap(cl, h, timelineSince(time.Minute)), "")
	checkAvailability(t, reading, h, []availabilityCase{
		{"", 4.0 / 9, [][2]int64{{4, 9}}, 1, 1},
		{"pack/a.bin", 1, nil, 0, 0},
		{"pack/sub/b.bin", 1.0 / 6, [][2]int64{{1, 6}}, 1, 1},
		{"pack/sub", 1.0 / 6, [][2]int64{{1, 6}}, 1, 1},
	})
	stopRead()
	<-readDone
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	// Warm-up: the same swarm with peers only just connected is not believed.
	fresh := NewStat(statMap(cl, h, timelineSince(0)), "")
	rep := statFor(t, fresh, h, "")
	if rep.GetAvailabilityKnown() || len(rep.GetMissing()) != 0 || rep.GetAvailability() != 4.0/9 || rep.GetWantedMissing() != 0 {
		t.Errorf("warm-up: known=%v missing=%v availability=%v wanted_missing=%d, want false/none/4/9 as a lower bound/0",
			rep.GetAvailabilityKnown(), rangesOf(rep), rep.GetAvailability(), rep.GetWantedMissing())
	}
	// ... except for a scope with nothing missing: the first file is all on
	// the peer, and more announcements cannot change that.
	if rep := statFor(t, fresh, h, "pack/a.bin"); !rep.GetAvailabilityKnown() || rep.GetAvailability() != 1 {
		t.Errorf("warm-up, fully announced file: known=%v availability=%v, want true/1", rep.GetAvailabilityKnown(), rep.GetAvailability())
	}
	// A torrent the map does not track has no clock: unknown.
	untracked := NewStat(statMap(cl, h, nil), "")
	if rep := statFor(t, untracked, h, ""); rep.GetAvailabilityKnown() {
		t.Error("a torrent without a peer timeline reported availability as known")
	}

	// Wanting the whole second file: its five pieces past the peer's are
	// wanted with no source, and no reader is on them. Its first piece, 3,
	// is also the first file's, and is on the peer.
	tor.Files()[1].SetPriority(torrent.PiecePriorityNormal)
	later := NewStat(statMap(cl, h, timelineSince(time.Minute)), "")
	for path, want := range map[string]int32{"": 5, "pack/sub/b.bin": 5, "pack/a.bin": 0} {
		rep := statFor(t, later, h, path)
		if got := rep.GetWantedMissing(); got != want {
			t.Errorf("%q: wanted_missing=%d, want %d", path, got, want)
		}
		if got := rep.GetReaderMissing(); got != 0 {
			t.Errorf("%q: reader_missing=%d with no reader open, want 0", path, got)
		}
	}
}

// Pieces complete here count as available and are never missing, though no
// connected peer has them: the seeder got piece 8 from a seeder that has
// since left, and the only peer now holds 0..3.
func TestStat_AvailabilityCountsLocalPieces(t *testing.T) {
	data, mi := availabilityTestPayload(t)
	seeder := swarmPeer(t, data, mi, len(data), 9)
	partial := swarmPeer(t, data, mi, 4*addTestPieceLen, 4)
	cl := mmapClient(t, t.TempDir(), false)
	tor, err := addTorrent(cl, mi)
	if err != nil {
		t.Fatal(err)
	}
	tor.DownloadPieces(8, 9)
	tor.AddClientPeer(seeder)
	waitFor(t, "piece 8 from the seeder", func() bool { return tor.Piece(8).State().Complete })
	seeder.Close()
	waitFor(t, "the seeder to go", func() bool { return len(tor.PeerConns()) == 0 })
	// Wanting piece 7, which nobody has any more, keeps the next connection
	// up (swarmLeecher).
	tor.DownloadPieces(7, 8)
	tor.AddClientPeer(partial)
	waitForPeerPieces(t, tor, 4)
	h := tor.InfoHash().HexString()
	st := NewStat(statMap(cl, h, timelineSince(time.Minute)), "")
	// Torrent: here {8}, on the peer {0..3}. b.bin and pack/sub start at
	// piece 3: position 0 is on the peer, position 5 (piece 8) here, 4
	// (piece 7) wanted.
	checkAvailability(t, st, h, []availabilityCase{
		{"", 5.0 / 9, [][2]int64{{4, 8}}, 1, 0},
		{"pack/a.bin", 1, nil, 0, 0},
		{"pack/sub/b.bin", 2.0 / 6, [][2]int64{{1, 5}}, 1, 0},
		{"pack/sub", 2.0 / 6, [][2]int64{{1, 5}}, 1, 0},
	})
}

// A web seed serving the whole torrent is a source of every piece, like a
// connected seeder: known at once, nothing missing, whatever the peers have
// — the library requests from it the same way. One the library never
// requests from (a directory URL without a trailing '/', webseed.Client
// SetInfo) holds nothing and changes nothing. Its URL does not have to
// answer: a web seed claims its pieces the way a peer's HAVE_ALL does.
func TestStat_AvailabilityWebseeds(t *testing.T) {
	data, mi := availabilityTestPayload(t)
	for _, c := range []struct {
		name, url   string
		remote      int
		cases       []availabilityCase
		timelineAge time.Duration
	}{
		{"whole torrent, peers just connected", "http://127.0.0.1:1/", 9, []availabilityCase{
			{"", 1, nil, 0, 0},
			{"pack/sub/b.bin", 1, nil, 0, 0},
		}, 0},
		{"non-conforming directory URL", "http://127.0.0.1:1/pack", 0, []availabilityCase{
			{"", 4.0 / 9, [][2]int64{{4, 9}}, 1, 0},
			{"pack/sub/b.bin", 1.0 / 6, [][2]int64{{1, 6}}, 1, 0},
		}, time.Minute},
	} {
		t.Run(c.name, func(t *testing.T) {
			peerCl := swarmPeer(t, data, mi, 4*addTestPieceLen, 4)
			cl, tor := swarmLeecherOn(t, mmapClient(t, t.TempDir(), true), mi, peerCl, 4)
			tor.AddWebSeeds([]string{c.url})
			ws := tor.WebseedPeerConns()
			if len(ws) != 1 || ws[0].Stats().RemotePieceCount != c.remote {
				t.Fatalf("%d web seeds, want 1 claiming %d pieces", len(ws), c.remote)
			}
			h := tor.InfoHash().HexString()
			st := NewStat(statMap(cl, h, timelineSince(c.timelineAge)), "")
			checkAvailability(t, st, h, c.cases)
		})
	}
}

// A connected seeder makes everything available at once — no warm-up, no
// timeline needed, and no read of the peers.
func TestStat_AvailabilityWithSeeder(t *testing.T) {
	data, mi := availabilityTestPayload(t)
	seeder := swarmPeer(t, data, mi, len(data), 9)
	cl, tor := swarmLeecher(t, mi, seeder, 9)
	h := tor.InfoHash().HexString()
	deadline := time.Now().Add(10 * time.Second)
	for tor.Stats().ConnectedSeeders == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the seeder never counted as one")
		}
		time.Sleep(10 * time.Millisecond)
	}
	st := NewStat(statMap(cl, h, nil), "")
	for _, path := range []string{"", "pack/sub/b.bin", "pack/sub"} {
		rep := statFor(t, st, h, path)
		if rep.GetAvailability() != 1 || !rep.GetAvailabilityKnown() || len(rep.GetMissing()) != 0 || rep.GetWantedMissing() != 0 {
			t.Errorf("%q: availability=%v known=%v missing=%v wanted_missing=%d, want 1/true/none/0",
				path, rep.GetAvailability(), rep.GetAvailabilityKnown(), rangesOf(rep), rep.GetWantedMissing())
		}
	}
	if n := st.swarmReads.Load(); n != 0 {
		t.Errorf("read the swarm %d times with a seeder connected, want 0", n)
	}
}

// lockedWriter is an http.ResponseWriter + Flusher safe to read while
// StatStream writes to it.
type lockedWriter struct {
	mu  sync.Mutex
	buf strings.Builder
	h   http.Header
}

func (w *lockedWriter) Header() http.Header { return w.h }
func (w *lockedWriter) WriteHeader(int)     {}
func (w *lockedWriter) Flush()              {}
func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}
func (w *lockedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// End to end through StatStream: the first SSE frame for a file carries the
// file's availability under the names web-ui reads.
func TestStatStream_SSEAvailability(t *testing.T) {
	data, mi := availabilityTestPayload(t)
	peerCl := swarmPeer(t, data, mi, 4*addTestPieceLen, 4)
	cl, tor := swarmLeecher(t, mi, peerCl, 4)
	h := tor.InfoHash().HexString()
	st := NewStat(statMap(cl, h, timelineSince(time.Minute)), "")

	ctx, cancel := context.WithCancel(metadata.NewIncomingContext(context.Background(), metadata.MD{"info-hash": []string{h}}))
	defer cancel()
	w := &lockedWriter{h: http.Header{}}
	done := make(chan error, 1)
	go func() { done <- st.StatStream(&pb.StatRequest{Path: "pack/sub/b.bin"}, NewStatStreamServer(ctx, w, w)) }()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(w.String(), "event: statupdate") {
		if time.Now().After(deadline) {
			t.Fatal("no statupdate frame in 5 s")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("StatStream: %v", err)
	}
	f := sseFrames(t, w.String())[0]
	missing, _ := json.Marshal(f["missing"])
	// A float32 goes out in its shortest form, 0.16666667.
	avail, _ := f["availability"].(float64)
	if float32(avail) != float32(1.0/6) || f["availability_known"] != true ||
		string(missing) != `[{"end":6,"start":1}]` || f["wanted_missing"] != float64(1) {
		t.Errorf("frame availability=%v availability_known=%v missing=%s wanted_missing=%v, want 1/6, true, [1,6), 1",
			f["availability"], f["availability_known"], missing, f["wanted_missing"])
	}
}
