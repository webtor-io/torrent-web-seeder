package main

import (
	"bytes"
	"strings"
	"testing"
)

// A private tracker's key, wherever its URL carries one, stays out of the
// tracker list diagnose prints; scheme, host and port stay in.
func TestPrintTrackers(t *testing.T) {
	const key = "0123456789abcdef0123456789abcdef" // fake
	var out bytes.Buffer
	printTrackers(&out, []string{
		"https://tracker.invalid/announce?passkey=" + key,
		"http://tracker.invalid:8080/" + key + "/announce",
		"http://" + key + "@tracker.invalid/announce",
		"udp://192.0.2.1:6969/announce?" + key,
		"http://[::1/" + key, // url.Parse refuses it
	})
	got := out.String()
	want := `       Trackers: 5
         - https://tracker.invalid
         - http://tracker.invalid:8080
         - http://tracker.invalid
         - udp://192.0.2.1:6969
         - <unparsable url>
`
	if got != want {
		t.Errorf("printed\n%s\nwant\n%s", got, want)
	}
	if strings.Contains(got, key) {
		t.Errorf("the key was printed:\n%s", got)
	}
}
