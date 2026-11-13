package services

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

// sourceFixture writes a one-piece torrent with a fake passkey tracker, web
// seed and DHT node (tracker.invalid never resolves) into dir and returns its infohash.
func sourceFixture(t *testing.T, dir, name string, private bool) metainfo.Hash {
	t.Helper()
	pieces := make([]byte, 20)
	_, _ = rand.Read(pieces)
	info := metainfo.Info{Name: name, Length: 1024, PieceLength: 1024, Pieces: pieces}
	if private {
		info.Private = &private
	}
	ib, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	tr := "http://tracker.invalid/announce?passkey=" + name
	mi := metainfo.MetaInfo{
		InfoBytes:    ib,
		Announce:     tr,
		AnnounceList: metainfo.AnnounceList{{tr}},
		UrlList:      []string{"http://tracker.invalid/webseed/" + name},
		Nodes:        []metainfo.Node{"tracker.invalid:6881"},
	}
	var b bytes.Buffer
	if err := mi.Write(&b); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".torrent"), b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return mi.HashInfoBytes()
}

// sourceClient is an offline TorrentClient in a temp dir, web seeds on, so
// url-list reaches a loaded torrent's metainfo. With no reader nothing is
// wanted and nothing is fetched.
func sourceClient(t *testing.T) *TorrentClient {
	t.Helper()
	dataDir := t.TempDir()
	tc := &TorrentClient{clientSettings: clientSettings{
		swarm:              newSwarmStats(),
		maxUnverifiedBytes: -1,
		dataDir:            dataDir,
		testConfig: func(cfg *torrent.ClientConfig) {
			loopbackConfig(cfg)
			cfg.DataDir = dataDir
			cfg.DisableWebseeds = false
		},
	}}
	t.Cleanup(tc.Close)
	return tc
}

func clientOf(t *testing.T, tc *TorrentClient) *torrent.Client {
	t.Helper()
	cl, err := tc.Get()
	if err != nil {
		t.Fatal(err)
	}
	return cl
}

// source.torrent of a private torrent goes to Vault only, without its
// trackers and web seeds; a public one keeps them. The request goes through
// WebSeeder.ServeHTTP; the public torrent is loaded into an offline client.
func TestRenderTorrent_PrivateWithoutTrackers(t *testing.T) {
	tc := sourceClient(t)
	torrentsDir := t.TempDir()
	priv := sourceFixture(t, torrentsDir, "priv", true)
	pub := sourceFixture(t, torrentsDir, "pub", false)
	srv := httptest.NewServer(&WebSeeder{tm: NewTorrentMap(tc, nil, &FileStoreMap{p: torrentsDir})})
	t.Cleanup(srv.Close)

	get := func(ih metainfo.Hash, role string) (int, http.Header, string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/"+ih.HexString()+"/"+SourceTorrentPath, nil)
		if role != "" {
			req.Header.Set("X-Role", role)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, resp.Header, string(body)
	}
	status := func(ih metainfo.Hash, role string) (int, string) {
		t.Helper()
		code, hdr, body := get(ih, role)
		// No X-Seeder-Reason: on swarm paths it says what an account lacks;
		// this 403 is about the role, whatever the account.
		if r := hdr.Get("X-Seeder-Reason"); r != "" {
			t.Errorf("source.torrent %v as %q: X-Seeder-Reason %q", ih, role, r)
		}
		return code, strings.TrimSpace(body)
	}
	getAs := func(ih metainfo.Hash, role string) (string, metainfo.MetaInfo) {
		t.Helper()
		code, _, body := get(ih, role)
		if code != http.StatusOK {
			t.Fatalf("source.torrent %v as %q: status %d", ih, role, code)
		}
		mi, err := metainfo.Load(strings.NewReader(body))
		if err != nil {
			t.Fatalf("source.torrent %v does not parse: %v", ih, err)
		}
		if mi.HashInfoBytes() != ih {
			t.Fatalf("source.torrent infohash %v, want %v", mi.HashInfoBytes(), ih)
		}
		return body, *mi
	}

	// A private torrent's source.torrent goes to Vault only (decision 28), built from
	// the stored metainfo: no client joins its swarm for it.
	if code, body := status(priv, ""); code != http.StatusForbidden || body != "private torrent" {
		t.Errorf("private source.torrent without X-Role: %d %q, want 403 \"private torrent\"", code, body)
	}
	body, mi := getAs(priv, "vault")
	if strings.Contains(body, "tracker.invalid") {
		t.Errorf("private source.torrent leaks trackers: announce %q, announce-list %v, url-list %v, nodes %v", mi.Announce, mi.AnnounceList, mi.UrlList, mi.Nodes)
	}
	if n := len(clientOf(t, tc).Torrents()); n != 0 {
		t.Errorf("%d torrents loaded to serve a private source.torrent, want 0", n)
	}

	body, mi = getAs(pub, "")
	if !strings.Contains(body, "passkey=pub") || len(mi.UrlList) != 1 {
		t.Errorf("public source.torrent lost its trackers: announce-list %v, url-list %v", mi.AnnounceList, mi.UrlList)
	}

	// A torrent without info (unknown privacy) goes out bare too.
	var noInfo metainfo.Hash
	_, _ = rand.Read(noInfo[:])
	nt, _ := clientOf(t, tc).AddTorrentInfoHash(noInfo)
	nt.AddTrackers([][]string{{"http://tracker.invalid/announce?passkey=noinfo"}})
	if got := sourceMetainfo(nt); len(got.AnnounceList) != 0 {
		t.Errorf("source metainfo without info keeps trackers: %v", got.AnnounceList)
	}
}

// The index of a private torrent is built from its info: it needs no account
// (thp decides who may see it) and joins no swarm.
func TestRenderTorrentIndex_PrivateFromMetaInfo(t *testing.T) {
	tc := sourceClient(t)
	torrentsDir := t.TempDir()
	priv := sourceFixture(t, torrentsDir, "priv", true)
	tm := NewTorrentMap(tc, nil, &FileStoreMap{p: torrentsDir}).WithPrivate(nil, true)
	srv := httptest.NewServer(&WebSeeder{tm: tm})
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/" + priv.HexString() + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), ">priv</a>") {
		t.Errorf("private index without X-Uid: %d %q, want 200 listing priv", resp.StatusCode, body)
	}
	if n := len(clientOf(t, tc).Torrents()); n != 0 {
		t.Errorf("%d torrents loaded to serve a private index, want 0", n)
	}
}
