package services

import (
	"testing"

	"github.com/anacrolix/torrent/metainfo"
)

// Two files in a directory plus one at the root, 10-byte pieces:
//
//	pieces 0..3  = "Show/a.mkv" (35 B) — pieces 0,1,2 and 5 B of piece 3
//	piece  3..4  = "Show/b.srt" (15 B) — 5 B of piece 3 and piece 4
//	pieces 5..6  = "readme.txt" (20 B)
func coldTestInfo() *metainfo.Info {
	return &metainfo.Info{
		Name:        "Pack",
		PieceLength: 10,
		Pieces:      make([]byte, 7*20),
		Files: []metainfo.FileInfo{
			{Path: []string{"Show", "a.mkv"}, Length: 35},
			{Path: []string{"Show", "b.srt"}, Length: 15},
			{Path: []string{"readme.txt"}, Length: 20},
		},
	}
}

func TestColdReply(t *testing.T) {
	info := coldTestInfo()
	// pieces 0,1 and 4 complete
	complete := []bool{true, true, false, false, true, false, false}
	cases := []struct {
		path      string
		total     int64
		completed int64
		pieces    int
		firstDone bool
	}{
		{"", 70, 30, 7, true},                 // whole: 10+10+10
		{"Pack/Show/a.mkv", 35, 20, 4, true},  // pieces 0,1 fully inside; 4 is outside
		{"Pack/Show/b.srt", 15, 10, 2, false}, // piece 3 (no), piece 4 (yes, 10 B inside)
		{"Pack/Show", 50, 30, 5, true},        // dir span 0..50 over pieces 0..4: 10+10+10
		{"Pack/readme.txt", 20, 0, 2, false},
	}
	for _, c := range cases {
		rep, err := coldReply(info, complete, c.path)
		if err != nil {
			t.Fatalf("%q: %v", c.path, err)
		}
		if rep.GetTotal() != c.total || rep.GetCompleted() != c.completed || len(rep.GetPieces()) != c.pieces {
			t.Errorf("%q: total=%d completed=%d pieces=%d, want %d/%d/%d", c.path, rep.GetTotal(), rep.GetCompleted(), len(rep.GetPieces()), c.total, c.completed, c.pieces)
		}
		if rep.GetLive() {
			t.Errorf("%q: cold reply must not claim to be live", c.path)
		}
		if rep.GetPeers() != 0 || rep.GetSeeders() != 0 {
			t.Errorf("%q: cold reply invented peers", c.path)
		}
		if got := rep.GetPieces()[0].GetComplete(); got != c.firstDone {
			t.Errorf("%q: first piece complete=%v want %v", c.path, got, c.firstDone)
		}
		if rep.GetPieces()[0].GetPosition() != 0 {
			t.Errorf("%q: positions must be relative to the span", c.path)
		}
	}
	if _, err := coldReply(info, complete, "Pack/nope.bin"); err == nil {
		t.Fatal("unknown path must be NotFound")
	}
	if rep, _ := coldReply(info, nil, ""); rep.GetCompleted() != 0 {
		t.Fatal("no completion data must mean nothing completed")
	}
}

func TestColdSpanSingleFile(t *testing.T) {
	info := &metainfo.Info{Name: "movie.mkv", PieceLength: 16, Length: 40, Pieces: make([]byte, 3*20)}
	if sp, ok := coldSpanFor(info, "movie.mkv"); !ok || sp.offset != 0 || sp.length != 40 {
		t.Fatalf("single-file torrent addressed by its name: %+v %v", sp, ok)
	}
	rep, err := coldReply(info, []bool{true, false, true}, "movie.mkv")
	if err != nil || rep.GetCompleted() != 16+8 {
		t.Fatalf("completed=%d err=%v, want 24 (piece 0 = 16, last piece = 8)", rep.GetCompleted(), err)
	}
}
