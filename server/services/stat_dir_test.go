package services

import "testing"

// A directory's stats are its files' stats summed; the boundary is the
// slash, so a sibling whose name merely starts with the directory's name
// stays out.
func TestIsUnderDir(t *testing.T) {
	cases := []struct {
		file, dir string
		want      bool
	}{
		{"Rio (2011)/Rio.mkv", "Rio (2011)", true},
		{"Rio (2011)/Subs/en.srt", "Rio (2011)", true},
		{"Rio (2011)/Subs/en.srt", "Rio (2011)/Subs", true},
		{"Rio (2011) extras/x.mkv", "Rio (2011)", false},
		{"Rio (2011)", "Rio (2011)", false}, // a file is not under itself
		{"x.mkv", "", false},                // "" is the whole torrent, handled before
	}
	for _, c := range cases {
		if got := isUnderDir(c.file, c.dir); got != c.want {
			t.Errorf("isUnderDir(%q, %q) = %v, want %v", c.file, c.dir, got, c.want)
		}
	}
}

func TestDirPieceRange(t *testing.T) {
	cases := []struct {
		pieceLen, offset, length int64
		begin, end               int
	}{
		{1024, 0, 1024, 0, 1},
		{1024, 0, 1025, 0, 2},
		{1024, 1000, 100, 0, 2},
		{1024, 2048, 4096, 2, 6},
		{1024, 3000, 0, 0, 0},
		{0, 0, 10, 0, 0},
	}
	for _, c := range cases {
		b, e := dirPieceRange(c.pieceLen, c.offset, c.length)
		if b != c.begin || e != c.end {
			t.Errorf("dirPieceRange(%d,%d,%d) = [%d,%d), want [%d,%d)", c.pieceLen, c.offset, c.length, b, e, c.begin, c.end)
		}
	}
}
