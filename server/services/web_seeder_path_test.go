package services

import "testing"

func TestSamePathToleratesEmptyComponents(t *testing.T) {
	yes := [][2]string{
		{"Name/a.mp3", "Name/a.mp3"},
		{"Name//a.mp3", "Name/a.mp3"},
		{"Name//sub//a.mp3", "Name/sub/a.mp3"},
		{"Name/a.mp3", "/Name/a.mp3"},
		{"Name//a.mp3", "Name//a.mp3"},
	}
	for _, c := range yes {
		if !samePath(c[0], c[1]) {
			t.Errorf("samePath(%q, %q) = false, want true", c[0], c[1])
		}
	}
	no := [][2]string{
		{"Name/a.mp3", "Name/b.mp3"},
		{"Name/a.mp3", "Other/a.mp3"},
		{"Name/sub/a.mp3", "Name/a.mp3"},
	}
	for _, c := range no {
		if samePath(c[0], c[1]) {
			t.Errorf("samePath(%q, %q) = true, want false", c[0], c[1])
		}
	}
}
