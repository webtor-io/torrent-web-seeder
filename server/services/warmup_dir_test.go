package services

import "testing"

// A directory target is its files laid end to end; a range over it must
// land on the right files with offsets relative to each file.
func TestWarmTargetEach(t *testing.T) {
	wt := &warmTarget{segs: []warmSegment{{off: 0, n: 100}, {off: 100, n: 50}, {off: 150, n: 200}}, length: 350}
	type hit struct{ seg, fs, fe int64 }
	collect := func(start, end int64) (out []hit) {
		wt.each(start, end, func(s warmSegment, fs, fe int64) { out = append(out, hit{s.off, fs, fe}) })
		return
	}
	cases := []struct {
		name       string
		start, end int64
		want       []hit
	}{
		{"head inside first file", 0, 9, []hit{{0, 0, 9}}},
		{"spans first two files", 90, 119, []hit{{0, 90, 99}, {100, 0, 19}}},
		{"whole target", 0, 349, []hit{{0, 0, 99}, {100, 0, 49}, {150, 0, 199}}},
		{"tail of last file", 340, 349, []hit{{150, 190, 199}}},
		{"exactly one middle file", 100, 149, []hit{{100, 0, 49}}},
	}
	for _, c := range cases {
		got := collect(c.start, c.end)
		if len(got) != len(c.want) {
			t.Fatalf("%s: got %v want %v", c.name, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("%s: got %v want %v", c.name, got, c.want)
			}
		}
	}
}
