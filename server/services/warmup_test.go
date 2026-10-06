package services

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/storage"
	logrusmiddleware "github.com/bakins/logrus-middleware"
	log "github.com/sirupsen/logrus"
)

func TestParseSingleRange(t *testing.T) {
	const fileLen = int64(1000)
	tests := []struct {
		name      string
		header    string
		fileLen   int64
		wantStart int64
		wantEnd   int64
		wantErr   bool
	}{
		{name: "empty header → whole file", header: "", fileLen: fileLen, wantStart: 0, wantEnd: 999},
		{name: "closed range", header: "bytes=0-99", fileLen: fileLen, wantStart: 0, wantEnd: 99},
		{name: "single byte", header: "bytes=0-0", fileLen: fileLen, wantStart: 0, wantEnd: 0},
		{name: "middle range", header: "bytes=200-499", fileLen: fileLen, wantStart: 200, wantEnd: 499},
		{name: "open-ended start- → file end", header: "bytes=500-", fileLen: fileLen, wantStart: 500, wantEnd: 999},
		{name: "suffix range -N", header: "bytes=-100", fileLen: fileLen, wantStart: 900, wantEnd: 999},
		{name: "suffix range larger than file is clamped", header: "bytes=-5000", fileLen: fileLen, wantStart: 0, wantEnd: 999},
		{name: "end past EOF is clamped", header: "bytes=0-99999", fileLen: fileLen, wantStart: 0, wantEnd: 999},
		{name: "multi-range honours first only", header: "bytes=0-99,200-299", fileLen: fileLen, wantStart: 0, wantEnd: 99},
		{name: "whitespace tolerated", header: "bytes= 100 - 199 ", fileLen: fileLen, wantStart: 100, wantEnd: 199},

		{name: "empty file errors", header: "", fileLen: 0, wantErr: true},
		{name: "wrong unit errors", header: "kb=0-99", fileLen: fileLen, wantErr: true},
		{name: "no dash errors", header: "bytes=100", fileLen: fileLen, wantErr: true},
		{name: "both empty errors", header: "bytes=-", fileLen: fileLen, wantErr: true},
		{name: "non-numeric errors", header: "bytes=abc-def", fileLen: fileLen, wantErr: true},
		{name: "end before start errors", header: "bytes=500-100", fileLen: fileLen, wantErr: true},
		{name: "start past EOF errors", header: "bytes=1000-", fileLen: fileLen, wantErr: true},
		{name: "negative start errors", header: "bytes=-0", fileLen: fileLen, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end, err := parseSingleRange(tt.header, tt.fileLen)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got start=%d end=%d", start, end)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if start != tt.wantStart || end != tt.wantEnd {
				t.Fatalf("got [%d,%d], want [%d,%d]", start, end, tt.wantStart, tt.wantEnd)
			}
		})
	}
}

// fpState builds a torrent.FilePieceState for tests. Bytes is the number
// of file bytes covered by this piece (matches FilePieceState semantics:
// boundary pieces only count the slice belonging to the file).
func fpState(bytes int64, complete bool) torrent.FilePieceState {
	return torrent.FilePieceState{
		Bytes: bytes,
		PieceState: torrent.PieceState{
			Completion: storage.Completion{Ok: true, Complete: complete},
		},
	}
}

func TestWarmupBytes(t *testing.T) {
	// Reference layout: 4 pieces × 100 bytes = 400-byte file. Some tests
	// override this with a custom slice when they need an odd last piece
	// or an offset first piece.
	uniform := func(complete ...bool) []torrent.FilePieceState {
		out := make([]torrent.FilePieceState, len(complete))
		for i, c := range complete {
			out[i] = fpState(100, c)
		}
		return out
	}

	tests := []struct {
		name       string
		state      []torrent.FilePieceState
		rangeStart int64
		rangeEnd   int64
		want       int64
	}{
		{
			name:       "empty state → 0",
			state:      nil,
			rangeStart: 0, rangeEnd: 99,
			want: 0,
		},
		{
			name:       "single piece, complete, exact range",
			state:      uniform(true),
			rangeStart: 0, rangeEnd: 99,
			want: 100,
		},
		{
			name:       "single piece, incomplete",
			state:      uniform(false),
			rangeStart: 0, rangeEnd: 99,
			want: 0,
		},
		{
			name:       "whole file, all complete",
			state:      uniform(true, true, true, true),
			rangeStart: 0, rangeEnd: 399,
			want: 400,
		},
		{
			name:       "whole file, half complete (first half)",
			state:      uniform(true, true, false, false),
			rangeStart: 0, rangeEnd: 399,
			want: 200,
		},
		{
			name:       "range crossing 3 pieces, middle missing",
			state:      uniform(true, false, true, true),
			rangeStart: 50, rangeEnd: 249,
			// piece 0: bytes 50..99 (50), piece 1: skipped, piece 2: bytes 200..249 (50)
			want: 100,
		},
		{
			name:       "range crossing 3 pieces, all complete",
			state:      uniform(true, true, true, true),
			rangeStart: 50, rangeEnd: 249,
			// piece 0: 50, piece 1: 100, piece 2: 50
			want: 200,
		},
		{
			name:       "range fully inside one piece",
			state:      uniform(true, true, true, true),
			rangeStart: 130, rangeEnd: 170,
			want: 41,
		},
		{
			name:       "range fully inside one incomplete piece",
			state:      uniform(true, false, true, true),
			rangeStart: 130, rangeEnd: 170,
			want: 0,
		},
		{
			name:       "ignores pieces outside range",
			state:      uniform(true, true, true, true),
			rangeStart: 100, rangeEnd: 199,
			want: 100,
		},
		{
			name: "tail-style file with short final piece",
			// File is 350 bytes: 3 full pieces (100 each) + 1 partial (50).
			state:      []torrent.FilePieceState{fpState(100, true), fpState(100, true), fpState(100, true), fpState(50, true)},
			rangeStart: 0, rangeEnd: 349,
			want: 350,
		},
		{
			name:       "tail piece only, complete",
			state:      []torrent.FilePieceState{fpState(100, false), fpState(100, false), fpState(100, false), fpState(50, true)},
			rangeStart: 300, rangeEnd: 349,
			want: 50,
		},
		{
			name: "file straddles a piece boundary (head piece is partial)",
			// Mimics File.State() when the file starts mid-piece: first
			// FilePieceState reports only the file-bytes inside that piece.
			state:      []torrent.FilePieceState{fpState(60, true), fpState(100, true), fpState(40, true)},
			rangeStart: 0, rangeEnd: 199,
			want: 200,
		},
		{
			name:       "range collapses to single byte on a piece boundary",
			state:      uniform(true, true, true, true),
			rangeStart: 100, rangeEnd: 100,
			want: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := warmupBytes(tt.state, tt.rangeStart, tt.rangeEnd)
			if got != tt.want {
				t.Fatalf("warmupBytes = %d, want %d", got, tt.want)
			}
		})
	}
}

// A range's pieces are whole at its edges and each counted once: in a v1
// directory a file that ends mid-piece shares that piece with the next, and
// counting it per file put its length into span twice.
func TestWarmTargetPieces(t *testing.T) {
	// 100-byte pieces. v1: b follows a at torrent byte 150, piece 1 holds
	// both. v2: b starts on the next piece boundary, 200.
	v1 := &warmTarget{segs: []warmSegment{{off: 0, n: 150, at: 0}, {off: 150, n: 150, at: 150}}, length: 300}
	v2 := &warmTarget{segs: []warmSegment{{off: 0, n: 150, at: 0}, {off: 150, n: 150, at: 200}}, length: 300}
	cases := []struct {
		name       string
		wt         *warmTarget
		start, end int64
		want       []int
	}{
		{"inside one piece", v1, 120, 130, []int{1}},
		{"a byte on a boundary", v1, 100, 100, []int{1}},
		{"edges take whole pieces", v1, 50, 250, []int{0, 1, 2}},
		{"a shared piece once", v1, 0, 299, []int{0, 1, 2}},
		{"the second file from its shared piece", v1, 150, 299, []int{1, 2}},
		{"v2 whole", v2, 0, 299, []int{0, 1, 2, 3}},
		{"v2 second file", v2, 150, 299, []int{2, 3}},
	}
	for _, c := range cases {
		if got := c.wt.pieces(100, c.start, c.end); !slices.Equal(got, c.want) {
			t.Errorf("%s: pieces(%d, %d) = %v, want %v", c.name, c.start, c.end, got, c.want)
		}
	}
}

// have counts every chunk written, hashed or not; a failed hash clears the
// piece's chunks and have drops.
func TestPieceSpanAndHave(t *testing.T) {
	pieces := []int{3, 4, 5}
	length := map[int]int64{3: 100, 4: 100, 5: 40} // 5 is the torrent's short last piece
	missing := map[int]int64{3: 0, 4: 50, 5: 40}   // 3 verified, 4 half written, 5 nothing
	span := pieceSpan(pieces, func(i int) int64 { return length[i] })
	if span != 240 {
		t.Fatalf("span = %d, want 240", span)
	}
	have := func() int64 { return pieceHave(pieces, span, func(i int) int64 { return missing[i] }) }
	if got := have(); got != 150 {
		t.Errorf("have = %d, want 150: piece 3 whole and piece 4's written half", got)
	}
	missing[4] = 100 // its hash failed
	if got := have(); got != 100 {
		t.Errorf("have after piece 4's failed hash = %d, want 100", got)
	}
}

// A torrent whose geometry the library panics on (metainfo.Piece.Length)
// gets no span, and the stream keeps its data: lines.
func TestPieceSpanSurvivesAPanickingLength(t *testing.T) {
	span := pieceSpan([]int{0, 1}, func(i int) int64 {
		if i == 1 {
			panic(int64(487621536278))
		}
		return 100
	})
	if span != 0 {
		t.Errorf("span = %d, want 0", span)
	}
}

// ?warmup's have counts a piece's chunks before its hash: a range whose one
// piece is all written and not yet verified reads have = span with data 0.
// The test holds the library's MarkNotComplete of a piece a bad peer sent
// (unverified_serve_test.go's harness), the "written, not yet rejected"
// state; on release the chunks are cleared and have drops; the good peer
// then completes the piece and the stream closes on data = total, as
// before. The range starts and ends inside the piece: span is the whole
// piece, data only the range.
func TestWarmupReportsUnverifiedBytes(t *testing.T) {
	const (
		pieceLen = 64 << 10
		pieces   = 4
		bad      = 1
	)
	data, mi := multiChunkPayload(t, pieceLen, pieces)
	ih := mi.HashInfoBytes()
	corrupt := bytes.Clone(data)
	clear(corrupt[bad*pieceLen+16<<10 : bad*pieceLen+32<<10])
	badPeer := seedModeClient(t, corrupt, mi)
	goodPeer := seedingClient(t, data, mi)

	dataDir := t.TempDir()
	hold := &holdHashFailure{piece: bad, failed: make(chan struct{}), release: make(chan struct{})}
	tc := &TorrentClient{
		swarm:              newSwarmStats(),
		rLimit:             -1,
		maxUnverifiedBytes: -1,
		dataDir:            dataDir,
		testConfig: func(cfg *torrent.ClientConfig) {
			loopbackConfig(cfg)
			hold.ClientImpl = cfg.DefaultStorage
			cfg.DefaultStorage = hold
		},
	}
	t.Cleanup(tc.Close)
	t.Cleanup(hold.unblock)
	torrentsDir := t.TempDir()
	tf, err := os.Create(filepath.Join(torrentsDir, "payload.torrent"))
	if err != nil {
		t.Fatal(err)
	}
	if err := mi.Write(tf); err != nil {
		t.Fatal(err)
	}
	_ = tf.Close()
	tm := NewTorrentMap(tc, nil, &FileStoreMap{p: torrentsDir})
	wu := NewWarmup(tm)
	logger := log.New()
	logger.SetOutput(io.Discard)
	srv := httptest.NewServer((&logrusmiddleware.Middleware{Logger: logger}).Handler(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := wu.Serve(w, r, ih.HexString(), "payload.bin"); err != nil {
				t.Error(err)
			}
		}), ""))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	tor, err := tm.Get(ctx, ih.HexString())
	if err != nil || tor == nil {
		t.Fatalf("load torrent: %v", err)
	}
	tor.AddClientPeer(badPeer)

	const start, end = bad*pieceLen + 100, (bad+1)*pieceLen - 101
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	type frame struct {
		keys             []string
		have, span, data int64
	}
	frames := make(chan frame, 64)
	var raw bytes.Buffer // read once frames is closed
	go func() {
		defer close(frames)
		sc := bufio.NewScanner(io.TeeReader(resp.Body, &raw))
		var f frame
		for sc.Scan() {
			if sc.Text() == "" {
				frames <- f
				f = frame{}
				continue
			}
			k, v, _ := strings.Cut(sc.Text(), ": ")
			n, _ := strconv.ParseInt(v, 10, 64)
			f.keys = append(f.keys, k)
			switch k {
			case "have":
				f.have = n
			case "span":
				f.span = n
			case "data":
				f.data = n
			}
		}
	}()
	var seen []frame
	until := func(what string, ok func(frame) bool) {
		t.Helper()
		timeout := time.After(10 * time.Second)
		for {
			select {
			case f, open := <-frames:
				if !open {
					t.Fatalf("the stream closed before %s; frames %+v", what, seen)
				}
				seen = append(seen, f)
				if ok(f) {
					return
				}
			case <-timeout:
				t.Fatalf("no frame with %s in 10 s; frames %+v, piece runs %v", what, seen, tor.PieceStateRuns())
			}
		}
	}

	select {
	case <-hold.failed:
	case <-time.After(30 * time.Second):
		t.Fatalf("piece %d never failed its hash; piece runs %v", bad, tor.PieceStateRuns())
	}
	until("the written piece held, data 0", func(f frame) bool { return f.have == pieceLen && f.data == 0 })
	hold.unblock()
	until("have dropping after the failed hash", func(f frame) bool { return f.have < pieceLen })
	tor.AddClientPeer(goodPeer)
	for f := range frames {
		seen = append(seen, f)
	}

	const total = end - start + 1
	last := seen[len(seen)-1]
	if last.data != total || last.have != pieceLen {
		t.Errorf("last frame %+v, want data %d (the range) and have %d (its piece)", last, total, pieceLen)
	}
	for _, f := range seen {
		if !slices.Equal(f.keys, []string{"have", "span", "data"}) || f.span != pieceLen {
			t.Fatalf("frame %+v, want have, span %d, data", f, pieceLen)
		}
	}
	// web-ui before have/span (services/api/api.go Warmup) reads only
	// "data: " lines.
	var old []int64
	for _, line := range strings.Split(raw.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimPrefix(line, "data: "), 10, 64)
		if err != nil {
			t.Fatalf("the old parser fails on %q: %v", line, err)
		}
		old = append(old, n)
	}
	if len(old) != len(seen) || old[len(old)-1] != total {
		t.Errorf("the old parser reads %v from %d frames, want one value a frame ending at %d", old, len(seen), total)
	}
}
