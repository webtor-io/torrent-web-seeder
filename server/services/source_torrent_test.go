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

// sourceFixture writes a one-piece torrent with a fake passkey tracker and
// web seed (tracker.invalid never resolves) into dir and returns its infohash.
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

// source.torrent of a private torrent goes out without its trackers and web
// seeds; a public one keeps them. The request goes through
// WebSeeder.ServeHTTP to a torrent the map loaded into an offline client.
func TestRenderTorrent_PrivateWithoutTrackers(t *testing.T) {
	dataDir := t.TempDir()
	tc := &TorrentClient{
		swarm:              newSwarmStats(),
		rLimit:             -1,
		maxUnverifiedBytes: -1,
		dataDir:            dataDir,
		testConfig: func(cfg *torrent.ClientConfig) {
			loopbackConfig(cfg)
			cfg.DataDir = dataDir
			// Web seeds on, so url-list reaches the client's metainfo. With
			// no reader nothing is wanted and nothing is fetched.
			cfg.DisableWebseeds = false
		},
	}
	t.Cleanup(tc.Close)
	torrentsDir := t.TempDir()
	priv := sourceFixture(t, torrentsDir, "priv", true)
	pub := sourceFixture(t, torrentsDir, "pub", false)
	srv := httptest.NewServer(&WebSeeder{tm: NewTorrentMap(tc, nil, &FileStoreMap{p: torrentsDir})})
	t.Cleanup(srv.Close)

	get := func(ih metainfo.Hash) (string, metainfo.MetaInfo) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/"+ih.HexString()+"/"+SourceTorrentPath, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("source.torrent %v: status %d, %v", ih, resp.StatusCode, err)
		}
		mi, err := metainfo.Load(bytes.NewReader(body))
		if err != nil {
			t.Fatalf("source.torrent %v does not parse: %v", ih, err)
		}
		if mi.HashInfoBytes() != ih {
			t.Fatalf("source.torrent infohash %v, want %v", mi.HashInfoBytes(), ih)
		}
		return string(body), *mi
	}

	body, mi := get(priv)
	if strings.Contains(body, "tracker.invalid") {
		t.Errorf("private source.torrent leaks trackers: announce %q, announce-list %v, url-list %v", mi.Announce, mi.AnnounceList, mi.UrlList)
	}

	body, mi = get(pub)
	if !strings.Contains(body, "passkey=pub") || len(mi.UrlList) != 1 {
		t.Errorf("public source.torrent lost its trackers: announce-list %v, url-list %v", mi.AnnounceList, mi.UrlList)
	}

	// A torrent without info (unknown privacy) goes out bare too.
	cl, err := tc.Get()
	if err != nil {
		t.Fatal(err)
	}
	var noInfo metainfo.Hash
	_, _ = rand.Read(noInfo[:])
	nt, _ := cl.AddTorrentInfoHash(noInfo)
	nt.AddTrackers([][]string{{"http://tracker.invalid/announce?passkey=noinfo"}})
	if got := sourceMetainfo(nt); len(got.AnnounceList) != 0 {
		t.Errorf("source metainfo without info keeps trackers: %v", got.AnnounceList)
	}
}
