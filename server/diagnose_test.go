package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
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

// A private .torrent from a file announces none of its trackers: a stored copy
// carries the key of the account that uploaded it. A public one keeps them.
func TestDropPrivateTrackers(t *testing.T) {
	const tr = "http://tracker.invalid/announce?passkey=00000000000000000000000000000000"
	for _, private := range []bool{true, false} {
		info := metainfo.Info{Name: "x", PieceLength: 1024, Length: 1, Pieces: make([]byte, 20)}
		if private {
			info.Private = &private
		}
		mi := metainfo.MetaInfo{InfoBytes: bencode.MustMarshal(info), Announce: tr, AnnounceList: metainfo.AnnounceList{{tr}}, UrlList: []string{tr}}
		dropped := dropPrivateTrackers(&mi, &info)
		if dropped != private || (private && (mi.Announce != "" || mi.AnnounceList != nil || mi.UrlList != nil)) || (!private && mi.Announce != tr) {
			t.Errorf("private=%v: dropped=%v, left %q %v %v", private, dropped, mi.Announce, mi.AnnounceList, mi.UrlList)
		}
	}
}
