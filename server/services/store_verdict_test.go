package services

import (
	"context"
	"flag"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/urfave/cli"
	ts "github.com/webtor-io/torrent-store/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeStore is torrent-store's client with Pull answered by pull.
type fakeStore struct {
	ts.TorrentStoreClient
	pull func() (*ts.PullReply, error)
}

func (f *fakeStore) Pull(context.Context, *ts.PullRequest, ...grpc.CallOption) (*ts.PullReply, error) {
	return f.pull()
}

// storeMap is a TorrentMap whose torrent store answers every Pull with pull
// and that has no torrent client: enough for MetaInfo, not for Get.
func storeMap(pull func() (*ts.PullReply, error)) *TorrentMap {
	return NewTorrentMap(nil, NewTorrentStoreMap(&TorrentStore{cl: &fakeStore{pull: pull}, inited: true}), &FileStoreMap{})
}

// notStored is the store not having the torrent: serveFile goes on as if it
// had not asked.
func notStored() (*ts.PullReply, error) {
	return nil, status.Error(codes.NotFound, "unable to find torrent")
}

// TestStoreRefusalBeforeVaultAndCache: a torrent torrent-store refuses gets
// 403 for a file in Vault and for one whole on this disk, Vault's own read
// included; a store that does not refuse (no torrent, an error, no answer)
// leaves both served as before.
func TestStoreRefusalBeforeVaultAndCache(t *testing.T) {
	const (
		hash      = "f07c04a783450dba366e562c253ce56573d7535f"
		path      = "episode.ts"
		body      = "0123456789abcdefghij"
		vaultCopy = "http://s3.invalid/vault-copy"
	)
	var vaultAsked atomic.Int32
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vaultAsked.Add(1)
		if r.Method == http.MethodHead {
			return
		}
		http.Redirect(w, r, vaultCopy, http.StatusFound)
	}))
	t.Cleanup(vault.Close)
	addr := vault.Listener.Addr().(*net.TCPAddr)
	fs := flag.NewFlagSet("vault", flag.ContinueOnError)
	fs.String(vaultHostFlag, addr.IP.String(), "")
	fs.Int(vaultPortFlag, addr.Port, "")
	vc := cli.NewContext(nil, fs, nil)

	info := metainfo.Info{Name: path, PieceLength: 16 << 10, Length: int64(len(body)), Pieces: make([]byte, 20)}
	torrent := bencode.MustMarshal(metainfo.MetaInfo{InfoBytes: bencode.MustMarshal(info)})
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	old := storeCheckTimeout
	storeCheckTimeout = 100 * time.Millisecond
	t.Cleanup(func() { storeCheckTimeout = old })

	refused := func() (*ts.PullReply, error) {
		return nil, status.Errorf(codes.PermissionDenied, "restricted by the rightholder infoHash=%v", hash)
	}
	for _, c := range []struct {
		name    string
		pull    func() (*ts.PullReply, error)
		inVault bool
		role    string
		status  int // 302: Vault's copy, 200: the file from this disk
	}{
		{"refused, on disk", refused, false, "", http.StatusForbidden},
		{"refused, in Vault", refused, true, "", http.StatusForbidden},
		{"refused, Vault reading", refused, true, "vault", http.StatusForbidden},
		{"allowed, in Vault", func() (*ts.PullReply, error) { return &ts.PullReply{Torrent: torrent}, nil }, true, "", http.StatusFound},
		{"not stored, in Vault", notStored, true, "", http.StatusFound},
		{"not stored, on disk", notStored, false, "", http.StatusOK},
		{"store down, in Vault", func() (*ts.PullReply, error) { return nil, status.Error(codes.Unavailable, "connection refused") }, true, "", http.StatusFound},
		{"store down, on disk", func() (*ts.PullReply, error) { return nil, status.Error(codes.Unavailable, "connection refused") }, false, "", http.StatusOK},
		{"store hangs, on disk", func() (*ts.PullReply, error) { <-hang; return nil, status.Error(codes.DeadlineExceeded, "") }, false, "", http.StatusOK},
	} {
		t.Run(c.name, func(t *testing.T) {
			ws := cachedFileSeeder(t, hash, path, body)
			ws.tm = storeMap(c.pull)
			ws.cl = http.DefaultClient
			if c.inVault {
				ws.v = NewVault(vc, ws.cl)
			}
			vaultAsked.Store(0)
			r := httptest.NewRequest(http.MethodGet, "/"+hash+"/"+path+"?download=true", nil)
			if c.role != "" {
				r.Header.Set("X-Role", c.role)
			}
			w := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { ws.ServeHTTP(w, r); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the request waits for the store")
			}
			got, _ := io.ReadAll(w.Body)
			if w.Code != c.status {
				t.Fatalf("status %d, Location %q, body %q; want %d", w.Code, w.Header().Get("Location"), got, c.status)
			}
			switch c.status {
			case http.StatusForbidden:
				if w.Header().Get("Location") != "" || string(got) == body || vaultAsked.Load() != 0 {
					t.Errorf("Location %q, body %q, Vault asked %d times; want neither the copy nor the file", w.Header().Get("Location"), got, vaultAsked.Load())
				}
			case http.StatusFound:
				if w.Header().Get("Location") != vaultCopy {
					t.Errorf("Location %q, want Vault's copy", w.Header().Get("Location"))
				}
			case http.StatusOK:
				if string(got) != body {
					t.Errorf("body %q, want the file from this disk", got)
				}
			}
		})
	}
}
