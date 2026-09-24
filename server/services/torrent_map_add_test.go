package services

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"github.com/go-llsqlite/adapter"
	"github.com/go-llsqlite/adapter/sqlitex"
)

// These tests add a torrent the way TorrentMap.Get does (addTorrent) to a
// client with this service's storage (NewMMap), and a peer or a web seed
// serves it over loopback. Nothing leaves the host.

const addTestPieceLen = 16 << 10

// addTestPayload returns random content, eight full pieces and a short last
// one, with a single-file metainfo that hashes it.
func addTestPayload(t *testing.T) ([]byte, *metainfo.MetaInfo) {
	t.Helper()
	data := make([]byte, 8*addTestPieceLen+1000)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	var pieces []byte
	for off := 0; off < len(data); off += addTestPieceLen {
		h := sha1.Sum(data[off:min(off+addTestPieceLen, len(data))])
		pieces = append(pieces, h[:]...)
	}
	infoBytes, err := bencode.Marshal(metainfo.Info{
		Name:        "payload.bin",
		PieceLength: addTestPieceLen,
		Pieces:      pieces,
		Length:      int64(len(data)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return data, &metainfo.MetaInfo{InfoBytes: infoBytes}
}

func addTestConfig(dir string) *torrent.ClientConfig {
	cfg := torrent.NewDefaultClientConfig()
	// The library's connection writer can miss a wake-up: it takes its
	// condition channel only after releasing the client lock, so a request
	// update signalled in between (an unchoke, a piece turning wanted) waits
	// for its next idle wake, the keepalive timer. With the default minute,
	// 11 of ~390 transfers under -race sat interested, unchoked and without
	// a request, the library's own storage included, and those given the
	// time finished at exactly 60 s. A short timer bounds that to a fraction
	// of a second. It cannot hide a piece that is never requestable: no
	// wake-up requests one.
	cfg.KeepAliveTimeout = 250 * time.Millisecond
	cfg.DataDir = dir
	cfg.ListenHost = torrent.LoopbackListenHost
	cfg.ListenPort = 0
	cfg.NoDHT = true
	cfg.DisableTrackers = true
	cfg.NoDefaultPortForwarding = true
	cfg.DisableAcceptRateLimiting = true
	cfg.DisableUTP = true
	cfg.DisableIPv6 = true
	cfg.DisableWebtorrent = true
	cfg.DisableWebseeds = true
	return cfg
}

// seedingClient holds the payload complete, in the library's own storage.
func seedingClient(t *testing.T, data []byte, mi *metainfo.MetaInfo) *torrent.Client {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "payload.bin"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := addTestConfig(dir)
	cfg.Seed = true
	st := storage.NewFileOpts(storage.NewFileClientOpts{
		ClientBaseDir:   dir,
		PieceCompletion: storage.NewMapPieceCompletion(),
		UsePartFiles:    g.Some(false),
	})
	cfg.DefaultStorage = st
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cl.Close()
		_ = st.Close()
	})
	tor, err := cl.AddTorrent(mi)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-tor.Complete().On():
	case <-time.After(10 * time.Second):
		t.Fatal("the seeding client never verified its payload")
	}
	return cl
}

// mmapClient is a client with this service's storage rooted at dir.
func mmapClient(t *testing.T, dir string, webseeds bool) *torrent.Client {
	t.Helper()
	cfg := addTestConfig(dir)
	cfg.DefaultStorage = NewMMap(dir, 0, FileCacheConfig{})
	cfg.DisableWebseeds = !webseeds
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })
	return cl
}

func hashedBytes(tor *torrent.Torrent) int64 {
	s := tor.Stats()
	return s.BytesHashed.Int64()
}

// readAllWithin reads the whole torrent through a reader, as a stream does,
// and fails if that takes longer than d or returns other bytes than want.
func readAllWithin(t *testing.T, tor *torrent.Torrent, want []byte, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	r := tor.NewReader()
	defer r.Close()
	r.SetContext(ctx)
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read %d of %d bytes in %v: %v; piece runs %v, %d bytes hashed",
			len(got), len(want), d, err, tor.PieceStateRuns(), hashedBytes(tor))
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("read %d bytes that differ from the %d-byte payload", len(got), len(want))
	}
}

func completionDBPath(t *testing.T, dataDir string, ih metainfo.Hash) string {
	t.Helper()
	dir, err := GetDir(dataDir, ih.HexString())
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, ".torrent.db")
}

// completionRows reads the piece_completion table: index -> complete.
func completionRows(t *testing.T, path string) map[int]bool {
	t.Helper()
	db, err := sqlite.OpenConn(path, sqlite.OpenReadOnly)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close()
	rows := map[int]bool{}
	err = sqlitex.Exec(db, `select "index", complete from piece_completion`, func(stmt *sqlite.Stmt) error {
		rows[stmt.ColumnInt(0)] = stmt.ColumnInt(1) != 0
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// The incident of 2026-09-24: a torrent this pod has never seen must
// download from a peer that has it. At 8f0cf39 no piece of it was ever
// requested and this read hit its deadline at 0 bytes.
func TestAddTorrent_DownloadsFromPeer(t *testing.T) {
	data, mi := addTestPayload(t)
	seeder := seedingClient(t, data, mi)
	dir := t.TempDir()
	tor, err := addTorrent(mmapClient(t, dir, false), mi)
	if err != nil {
		t.Fatal(err)
	}
	tor.AddClientPeer(seeder)
	readAllWithin(t, tor, data, 20*time.Second)

	// What the session downloaded is recorded: the next load of this
	// torrent serves it without a download or a hash.
	rows := completionRows(t, completionDBPath(t, dir, tor.InfoHash()))
	for i := 0; i < tor.NumPieces(); i++ {
		if !rows[i] {
			t.Errorf("piece %d downloaded but not recorded complete", i)
		}
	}
}

// The self-hosted smoke's path: no peers, one BEP 19 web seed. The library
// makes its first web seed request on a 5 s timer, not on the read, so this
// takes about 5 s.
func TestAddTorrent_DownloadsFromWebseed(t *testing.T) {
	data, mi := addTestPayload(t)
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "payload.bin"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.FileServer(http.Dir(src)))
	t.Cleanup(srv.Close)
	// A trailing slash makes the client append the torrent's name.
	mi.UrlList = []string{srv.URL + "/"}
	tor, err := addTorrent(mmapClient(t, t.TempDir(), true), mi)
	if err != nil {
		t.Fatal(err)
	}
	readAllWithin(t, tor, data, 30*time.Second)
}

// Adding a torrent reads none of it. Every piece is known and not complete,
// so the library's initial check has nothing to hash. Before 8f0cf39 that
// check hashed the whole never-downloaded torrent (a 93k-piece one held a
// pod at its 5-core limit for 10 minutes) and each failed hash wrote a
// complete=0 row: the hash counter and the rows are both its trace.
func TestAddTorrent_NeverDownloadedIsNotHashed(t *testing.T) {
	_, mi := addTestPayload(t)
	dir := t.TempDir()
	tor, err := addTorrent(mmapClient(t, dir, false), mi)
	if err != nil {
		t.Fatal(err)
	}
	// The completion must be the persistent one. Its db sits in the
	// torrent's dir, which does not exist before the first write, and the
	// in-memory fallback forgets everything the session downloads.
	db := completionDBPath(t, dir, tor.InfoHash())
	if _, err := os.Stat(db); err != nil {
		t.Fatalf("no completion db after add: %v", err)
	}
	// Initial checks are queued inside AddTorrent; let the hashers drain
	// them before looking for their trace.
	deadline := time.Now().Add(5 * time.Second)
	for busy := true; busy; {
		busy = false
		for _, r := range tor.PieceStateRuns() {
			busy = busy || r.Checking || r.Marking
		}
		if busy && time.Now().After(deadline) {
			t.Fatalf("pieces still being checked after 5 s: %v", tor.PieceStateRuns())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := hashedBytes(tor); n != 0 {
		t.Errorf("hashed %d bytes on add, want 0", n)
	}
	if rows := completionRows(t, db); len(rows) != 0 {
		t.Errorf("%d completion rows after add, want 0: %v", len(rows), rows)
	}
	for i := 0; i < tor.NumPieces(); i++ {
		if ps := tor.PieceState(i); !ps.Ok || ps.Complete {
			t.Errorf("piece %d: Ok=%v Complete=%v, want known and not complete", i, ps.Ok, ps.Complete)
		}
	}
}

// When the completion db cannot be opened, OpenTorrent falls back to an
// in-memory completion that knows no piece, and the library requests a piece
// only once a hash has settled it. The initial check is that hash: slow on a
// big torrent, but it downloads. With the check disabled (8f0cf39) this
// torrent would never get a byte, which is why addTorrent must not disable it.
func TestAddTorrent_DownloadsWithoutCompletionDB(t *testing.T) {
	data, mi := addTestPayload(t)
	seeder := seedingClient(t, data, mi)
	dir := t.TempDir()
	// A directory where the db file goes: sqlite cannot open it.
	db := completionDBPath(t, dir, mi.HashInfoBytes())
	if err := os.MkdirAll(db, 0o755); err != nil {
		t.Fatal(err)
	}
	tor, err := addTorrent(mmapClient(t, dir, false), mi)
	if err != nil {
		t.Fatal(err)
	}
	tor.AddClientPeer(seeder)
	readAllWithin(t, tor, data, 20*time.Second)
	if fi, err := os.Stat(db); err != nil || !fi.IsDir() {
		t.Fatalf("%s is no longer a directory: the completion was not the fallback", db)
	}
	t.Logf("fallback completion: %d of %d bytes hashed", hashedBytes(tor), len(data))
}
