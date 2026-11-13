package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
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
		kept := mi.Announce == tr && reflect.DeepEqual(mi.AnnounceList, metainfo.AnnounceList{{tr}}) && reflect.DeepEqual(mi.UrlList, metainfo.UrlList{tr})
		if dropped != private || (private && (mi.Announce != "" || mi.AnnounceList != nil || mi.UrlList != nil)) || (!private && !kept) {
			t.Errorf("private=%v: dropped=%v, left %q %v %v", private, dropped, mi.Announce, mi.AnnounceList, mi.UrlList)
		}
	}
}

// diagnose loads a private .torrent file with no trackers, says why, and
// prints neither them nor the key; a public one keeps and lists them.
func TestLoadTorrentFile(t *testing.T) {
	const key = "00000000000000000000000000000000"
	const tr = "http://tracker.invalid/announce?passkey=" + key
	const warn = "[WARN] private torrent: its trackers are not announced"
	for _, private := range []bool{true, false} {
		info := metainfo.Info{Name: "x", PieceLength: 1024, Length: 1, Pieces: make([]byte, 20)}
		if private {
			info.Private = &private
		}
		path := filepath.Join(t.TempDir(), "x.torrent")
		b := bencode.MustMarshal(metainfo.MetaInfo{InfoBytes: bencode.MustMarshal(info), Announce: tr, AnnounceList: metainfo.AnnounceList{{tr}}, UrlList: metainfo.UrlList{tr}})
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		mi, err := loadTorrentFile(&out, path)
		if err != nil {
			t.Fatal(err)
		}
		got := out.String()
		cleared := mi.Announce == "" && mi.AnnounceList == nil && mi.UrlList == nil
		if cleared != private || strings.Contains(got, warn) != private || strings.Contains(got, "Trackers: 0\n") != private || strings.Contains(got, key) {
			t.Errorf("private=%v: left %q %v %v, printed\n%s", private, mi.Announce, mi.AnnounceList, mi.UrlList, got)
		}
	}
}
