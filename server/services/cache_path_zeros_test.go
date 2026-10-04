package services

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	sqlite "github.com/go-llsqlite/adapter"
	"github.com/go-llsqlite/adapter/sqlitex"
	"github.com/urfave/cli"
)

// "Serve file from cache" reads a file with a plain *os.File, past the
// evicted-piece guard in mmapStoragePiece.ReadAt, and a torrent's directory
// is shared by every pod on the node. These tests pin the ways a punched hole
// reached a client as data: through the cache path, and through another pod's
// storage.
//
// Layout: four files over four 16 KiB pieces.
//
//	piece 0: a (all of it) + first 100 bytes of b
//	piece 1: rest of b      + first 100 bytes of c
//	piece 2: rest of c
//	piece 3: d (one byte short of a piece)
//
// Pieces 0, 1 and 3 are on disk, so a, b and d are complete files and c is
// not. The cache budget is exactly what is on disk; c's piece arriving makes
// the LRU evict one piece, and the tests touch 0 and 3 so that it is piece 1:
// b's tail.
const zpl = 16 << 10

// zMI is zInfo's metainfo. Its infohash names the torrent's dir, so a pod
// that loads the torrent (zPod) finds what the tests put there.
var zMI = func() *metainfo.MetaInfo {
	b, err := bencode.Marshal(zInfo())
	if err != nil {
		panic(err)
	}
	return &metainfo.MetaInfo{InfoBytes: b}
}()

var (
	zIH   = zMI.HashInfoBytes()
	zHash = zIH.HexString()
)

func zInfo() *metainfo.Info {
	return &metainfo.Info{
		Name:        "pack",
		PieceLength: zpl,
		Pieces:      makeDummyPieces(4),
		Files: []metainfo.FileInfo{
			{Path: []string{"a.mkv"}, Length: zpl - 100},
			{Path: []string{"b.mkv"}, Length: zpl},
			{Path: []string{"c.mkv"}, Length: zpl + 100},
			{Path: []string{"d.mkv"}, Length: zpl - 1},
		},
	}
}

// zData is the torrent's content; no byte of it is zero.
func zData() []byte {
	b := make([]byte, 4*zpl-1)
	for i := range b {
		b[i] = byte(i%251) + 1
	}
	return b
}

func zWantB() []byte { return zData()[zpl-100 : 2*zpl-100] }

// gatePub parks every Publish until release is closed, reporting each one on
// entered first. The completion loop publishes from inside CompleteFile, so a
// parked Cached(a) holds the loop between its snapshot (GetCompletedFiles) and
// its next insert (CompleteFile(b)); Cached(d) marks the end of the tick.
type gatePub struct {
	entered chan string
	release chan struct{}
}

func newGatePub() *gatePub {
	return &gatePub{entered: make(chan string, 16), release: make(chan struct{})}
}

func (g *gatePub) Publish(subject string, data []byte) error {
	g.entered <- subject + " " + string(data)
	<-g.release
	return nil
}

// wait returns once want has been published, passing over anything else.
func (g *gatePub) wait(t *testing.T, want string) {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case got := <-g.entered:
			if got == want {
				return
			}
		case <-timeout:
			t.Fatalf("no publish of %q", want)
		}
	}
}

var (
	cachedA = `resource.cached {"resource_id":"` + zHash + `","file_idx":0}`
	cachedD = `resource.cached {"resource_id":"` + zHash + `","file_idx":3}`
)

func zWritePiece(t testing.TB, impl storage.TorrentImpl, info *metainfo.Info, i int) {
	t.Helper()
	p := info.Piece(i)
	sp := impl.Piece(p)
	if _, err := sp.WriteAt(zData()[p.Offset():p.Offset()+p.Length()], 0); err != nil {
		t.Fatal(err)
	}
	if err := sp.MarkComplete(); err != nil {
		t.Fatal(err)
	}
}

// zTouch reads a piece through the storage, as a torrent-path reader would;
// the LRU then counts it as in use and leaves it for last.
func zTouch(t *testing.T, impl storage.TorrentImpl, info *metainfo.Info, i int) {
	t.Helper()
	buf := make([]byte, 16)
	if _, err := impl.Piece(info.Piece(i)).ReadAt(buf, 0); err != nil {
		t.Fatal(err)
	}
}

// zStorage is the storage behind impl, to drive its eviction by hand.
func zStorage(impl storage.TorrentImpl) *mmapTorrentStorage {
	return impl.Piece(zInfo().Piece(0)).(mmapStoragePiece).t
}

// zSeed is a node where an earlier session downloaded pieces 0, 1 and 3 and
// the completion loop recorded a, b and d as complete files.
func zSeed(t testing.TB) string {
	t.Helper()
	dataDir := t.TempDir()
	info := zInfo()
	impl, err := NewMMap(dataDir, 0, FileCacheConfig{}).OpenTorrent(context.Background(), info, zIH)
	if err != nil {
		t.Fatal(err)
	}
	zWritePiece(t, impl, info, 0)
	zWritePiece(t, impl, info, 1)
	zWritePiece(t, impl, info, 3)
	if err := impl.Close(); err != nil {
		t.Fatal(err)
	}
	pc, err := NewPieceCompletion(filepath.Join(dataDir, zHash), info, zIH, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"pack/a.mkv", "pack/b.mkv", "pack/d.mkv"} {
		if err := pc.CompleteFile(f); err != nil {
			t.Fatal(err)
		}
	}
	_ = pc.Close()
	return dataDir
}

// zOpen opens the torrent with eviction on: the budget is what is on disk
// (pieces 0, 1, 3), the torrent is larger, so nothing is evicted at open.
func zOpen(t *testing.T, dataDir string, pub cachePublisher) storage.TorrentImpl {
	t.Helper()
	m := NewMMap(dataDir, 3*zpl-1, FileCacheConfig{})
	if pub != nil {
		m.events = &CacheEvents{p: pub}
	}
	impl, err := m.OpenTorrent(context.Background(), zInfo(), zIH)
	if err != nil {
		t.Fatal(err)
	}
	return impl
}

func zFCM(dataDir string) *FileCacheMap {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String(DataDirFlag, dataDir, "")
	return NewFileCacheMap(cli.NewContext(nil, fs, nil))
}

func zExec(t *testing.T, dataDir, query string, args ...any) {
	t.Helper()
	db, err := sqlite.OpenConn(filepath.Join(dataDir, zHash, ".torrent.db"), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := sqlitex.Exec(db, query, nil, args...); err != nil {
		t.Fatal(err)
	}
}

func zRow(t *testing.T, dataDir, path string) bool {
	t.Helper()
	db, err := sqlite.OpenConn(filepath.Join(dataDir, zHash, ".torrent.db"), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	found := false
	err = sqlitex.Exec(db, `select 1 from file_completion where "path"=?`, func(*sqlite.Stmt) error {
		found = true
		return nil
	}, path)
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// zServeB asks WebSeeder.serveFile for b, as the proxy (and Vault) would.
// Vault is nil, so the request reaches the cache path; ok is false when the
// cache path declines it and the request would go to the torrent.
func zServeB(t *testing.T, dataDir string, fcm *FileCacheMap) (body []byte, ok bool) {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String(DataDirFlag, dataDir, "")
	s := &WebSeeder{fcm: fcm, tom: NewTouchMap(cli.NewContext(nil, fs, nil))}
	f, release, err := fcm.Open(zHash, "pack/b.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if f == nil {
		return nil, false
	}
	release()
	w := httptest.NewRecorder()
	s.serveFile(w, httptest.NewRequest(http.MethodGet, "/"+zHash+"/pack/b.mkv", nil), zHash, "pack/b.mkv")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	return w.Body.Bytes(), true
}

func zZeroRun(got, want []byte) (from, to int) {
	from = -1
	for i := range want {
		if i < len(got) && got[i] != want[i] {
			if got[i] != 0 {
				return i, -2 // not a zero hole: different defect
			}
			if from < 0 {
				from = i
			}
			to = i + 1
		}
	}
	return from, to
}

// TestCachePathCompletionLoopResurrectsEvictedFile: the completion loop takes
// its snapshot of complete files (GetCompletedFiles), then inserts the rows
// one by one (CompleteFile). An eviction that lands between the two deletes
// b's row (uncompleteAffectedFiles) and punches the hole, and the loop then
// inserts b's row back from its stale snapshot; no later tick deletes it and
// a reload keeps it. The cache path served b with the hole as data, on every
// request, as long as it trusted the row.
func TestCachePathCompletionLoopResurrectsEvictedFile(t *testing.T) {
	dataDir := zSeed(t)
	info := zInfo()

	gate := newGatePub()
	impl := zOpen(t, dataDir, gate)
	// The loop's first tick has taken its snapshot {a, b} and is parked
	// inside CompleteFile(a), publishing.
	gate.wait(t, cachedA)

	// A torrent-path reader touches piece 0; c's piece arrives; the LRU is
	// over budget and evicts the idle piece 1 -- b's tail.
	zTouch(t, impl, info, 0)
	zTouch(t, impl, info, 3)
	zWritePiece(t, impl, info, 2)
	if zRow(t, dataDir, "pack/b.mkv") {
		t.Fatal("precondition: eviction of piece 1 must have deleted b's file_completion row")
	}

	// The loop resumes and finishes its tick from the snapshot.
	close(gate.release)
	gate.wait(t, cachedD)

	if body, ok := zServeB(t, dataDir, zFCM(dataDir)); ok && !bytes.Equal(body, zWantB()) {
		from, to := zZeroRun(body, zWantB())
		t.Errorf("cache path serves b with zeroes at [%d,%d) of %d after its piece was evicted (file_completion row re-inserted by the completion loop)", from, to, len(body))
	}

	// The torrent is unloaded and opened again (another request, or another
	// pod on the node). Its loop runs a full tick: a and d.
	if err := impl.Close(); err != nil {
		t.Fatal(err)
	}
	gate2 := newGatePub()
	close(gate2.release)
	impl2 := zOpen(t, dataDir, gate2)
	defer impl2.Close()
	gate2.wait(t, cachedD)

	if body, ok := zServeB(t, dataDir, zFCM(dataDir)); ok && !bytes.Equal(body, zWantB()) {
		from, to := zZeroRun(body, zWantB())
		t.Errorf("after reload the stale row is still there: cache path serves b with zeroes at [%d,%d)", from, to)
	}
}

// TestCachePathStreamOutlivesEviction: no race in the completion code at all.
// b is complete and is being streamed from the cache path through a plain
// *os.File when a pod opens the torrent with eviction on (the cache path
// leaves a torrent that is evicted already to the torrent, so the stream began
// before). Those reads never reach the storage, so the LRU never sees b in
// use, and its pieces stay the oldest idle ones -- the first to go when
// another reader of the same torrent pushes the cache over budget. The
// eviction punched the hole under the open descriptor and the stream read
// zeroes with a 200. FileCacheMap also kept answering with the path for its
// 60 s TTL after the row was gone.
func TestCachePathStreamOutlivesEviction(t *testing.T) {
	dataDir := zSeed(t)
	info := zInfo()

	fcm := zFCM(dataDir)
	if cp, err := fcm.Get(zHash, "pack/b.mkv"); err != nil || cp == "" {
		t.Fatalf("precondition: b must be cached (cp=%q err=%v)", cp, err)
	}
	f, release, err := fcm.Open(zHash, "pack/b.mkv") // what serveFile streams from
	if err != nil || f == nil {
		t.Fatalf("precondition: b must be served from cache (err=%v)", err)
	}
	defer release()
	head := make([]byte, 100) // the stream is under way
	if _, err := f.ReadAt(head, 0); err != nil {
		t.Fatal(err)
	}

	gate := newGatePub()
	close(gate.release)
	impl := zOpen(t, dataDir, gate)
	defer impl.Close()
	gate.wait(t, cachedD) // first tick done: a, b, d complete, rows present

	zTouch(t, impl, info, 0) // torrent-path readers of a and d
	zTouch(t, impl, info, 3)
	zWritePiece(t, impl, info, 2)

	tail := make([]byte, zpl-100)
	if _, err := f.ReadAt(tail, 100); err != nil {
		t.Fatal(err)
	}
	got := append(head, tail...)
	if !bytes.Equal(got, zWantB()) {
		from, to := zZeroRun(got, zWantB())
		t.Errorf("open cache-path stream read zeroes at [%d,%d) of b after an eviction under it", from, to)
	}

	// The stream ends; the eviction it held off goes through.
	release()
	zStorage(impl).evictOverBudget()
	if zRow(t, dataDir, "pack/b.mkv") {
		t.Fatal("the eviction put off by the stream did not happen after it")
	}
	if body, ok := zServeB(t, dataDir, fcm); ok && !bytes.Equal(body, zWantB()) {
		from, to := zZeroRun(body, zWantB())
		t.Errorf("new request within FileCacheMap's TTL is served from cache with zeroes at [%d,%d)", from, to)
	}
}

// TestCachePathSharedDirAcrossPods: every seeder pod on a node mounts the same
// hostPath and GetDir sends one infohash to one directory, so two pods that
// both have the torrent loaded share .torrent.db and the content files -- but
// each has its own evicted flags and its own in-memory completion. Pod 1
// evicted piece 1. Pod 2 still held b as complete: its storage reads of
// piece 1 returned zeroes with no error (the guard is per process), and its
// completion loop re-inserted b's row on its next tick, so the cache path
// served the hole on any pod of the node.
func TestCachePathSharedDirAcrossPods(t *testing.T) {
	dataDir := zSeed(t)
	info := zInfo()

	gate1 := newGatePub()
	close(gate1.release)
	pod1 := zOpen(t, dataDir, gate1)
	defer pod1.Close()
	gate1.wait(t, cachedD)

	gate2 := newGatePub()
	close(gate2.release)
	pod2 := zOpen(t, dataDir, gate2)
	gate2.wait(t, cachedD)

	zTouch(t, pod1, info, 0)
	zTouch(t, pod1, info, 3)
	zWritePiece(t, pod1, info, 2) // pod 1's LRU picks piece 1

	// Torrent path on pod 2: anacrolix there still has piece 1 complete and
	// reads it through the storage.
	buf := make([]byte, zpl)
	n, err := pod2.Piece(info.Piece(1)).ReadAt(buf, 0)
	if err == nil && !bytes.Equal(buf[:n], zData()[zpl : 2*zpl][:n]) {
		from, to := zZeroRun(buf[:n], zData()[zpl:2*zpl])
		t.Errorf("pod 2 storage read of piece 1 evicted by pod 1: err=nil, zeroes at [%d,%d)", from, to)
	}

	// Cache path: pod 2's loop re-inserted b on its next 5 s tick (real
	// ticker, piece_completion.go; waited for, not raced) when the row was
	// gone.
	deadline := time.Now().Add(12 * time.Second)
	for !zRow(t, dataDir, "pack/b.mkv") && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if body, ok := zServeB(t, dataDir, zFCM(dataDir)); ok && !bytes.Equal(body, zWantB()) {
		from, to := zZeroRun(body, zWantB())
		t.Errorf("pod 2's completion loop re-inserted b's row; cache path serves zeroes at [%d,%d)", from, to)
	}

	// Pod 2 lets go of the torrent: pod 1 may evict now.
	if err := pod2.Close(); err != nil {
		t.Fatal(err)
	}
	zStorage(pod1).evictOverBudget()
	if _, err := pod1.Piece(info.Piece(1)).ReadAt(buf, 0); !errors.Is(err, ErrPieceEvicted) {
		t.Fatalf("piece 1 not evicted once pod 2 let go of the torrent: %v", err)
	}
	if body, ok := zServeB(t, dataDir, zFCM(dataDir)); ok {
		from, to := zZeroRun(body, zWantB())
		t.Errorf("cache path serves b with piece 1 evicted, zeroes at [%d,%d)", from, to)
	}
}

// TestCrossPodRecoveryEviction: the production trigger of the above. Pod A
// has the torrent loaded; pod B loads it too, finds the node's cache over its
// budget on recovery and evicts right in OpenTorrent -- 2026-10-02 06:38:18
// glwq2 evicting 9093 of 071e75f1 while spmdr had it, 2026-10-04 00:58:19
// flqlt evicting 25812 of d2badd18 while kt6xk had it. Which pieces recovery
// evicts depends on map order, so every piece pod A has is read back.
func TestCrossPodRecoveryEviction(t *testing.T) {
	const pieceLen = 16 << 10
	const numPieces = 8 // a.bin = pieces 0..3, b.bin = pieces 4..7
	info := &metainfo.Info{
		PieceLength: pieceLen,
		Name:        "T",
		Pieces:      makeDummyPieces(numPieces),
		Files: []metainfo.FileInfo{
			{Path: []string{"a.bin"}, Length: 4 * pieceLen},
			{Path: []string{"b.bin"}, Length: 4 * pieceLen},
		},
	}
	var ih metainfo.Hash
	copy(ih[:], "xpodtest1234567890ab")
	base := t.TempDir()

	// Pod A: six pieces, within its budget.
	podA, err := NewMMap(base, 6*pieceLen, FileCacheConfig{}).OpenTorrent(context.Background(), info, ih)
	if err != nil {
		t.Fatal(err)
	}
	pat := func(i int) []byte { return bytes.Repeat([]byte{byte(0xa0 | i)}, pieceLen) }
	for i := 0; i < 6; i++ {
		p := podA.Piece(info.Piece(i))
		if _, err := p.WriteAt(pat(i), 0); err != nil {
			t.Fatal(err)
		}
		if err := p.MarkComplete(); err != nil {
			t.Fatal(err)
		}
	}

	// Pod B: same node, same dir, a budget of four pieces: two over.
	podB, err := NewMMap(base, 4*pieceLen, FileCacheConfig{}).OpenTorrent(context.Background(), info, ih)
	if err != nil {
		t.Fatal(err)
	}
	defer podB.Close()

	buf := make([]byte, pieceLen)
	for i := 0; i < 6; i++ {
		n, err := podA.Piece(info.Piece(i)).ReadAt(buf, 0)
		if err == nil && !bytes.Equal(buf[:n], pat(i)[:n]) {
			t.Errorf("pod A read piece %d after pod B's recovery: n=%d err=nil, not the piece's bytes (all zero: %v)",
				i, n, bytes.Count(buf[:n], []byte{0}) == n)
		}
	}

	// Pod A lets go; pod B's next attempt evicts its two pieces.
	if err := podA.Close(); err != nil {
		t.Fatal(err)
	}
	tsB := podB.Piece(info.Piece(0)).(mmapStoragePiece).t
	tsB.evictOverBudget()
	if used := tsB.lru.Used(); used > 4*pieceLen {
		t.Fatalf("pod B still over budget once alone: %d > %d", used, 4*pieceLen)
	}
}

// TestCachePathLegacyRow: a file_completion row from before the pieces
// columns says nothing the cache path can check, and such rows are where
// production's zeros sat (071e75f1 on worker64: the row present, piece 9093
// punched). The cache path leaves the file to the torrent until a completion
// loop writes the row again.
func TestCachePathLegacyRow(t *testing.T) {
	dataDir := zSeed(t)
	db := filepath.Join(dataDir, zHash)
	// Piece 1 evicted, b's row put back: the piece marked incomplete and
	// b's part of it zeroed, as a punch leaves it.
	zExec(t, dataDir, `update piece_completion set complete = 0 where "index" = 1`)
	if err := os.WriteFile(cachedFilePath(db, "pack/b.mkv"), append(zWantB()[:100], make([]byte, zpl-100)...), 0o644); err != nil {
		t.Fatal(err)
	}

	// A db no storage has opened since the columns: the old table.
	zExec(t, dataDir, `drop table file_completion`)
	zExec(t, dataDir, `create table file_completion("path", unique("path"))`)
	zExec(t, dataDir, `insert into file_completion values('pack/b.mkv')`)
	if body, ok := zServeB(t, dataDir, zFCM(dataDir)); ok {
		from, to := zZeroRun(body, zWantB())
		t.Errorf("legacy db: cache path serves b, zeroes at [%d,%d)", from, to)
	}

	// Opened since: the columns are there and NULL in the old row.
	pc, err := NewPieceCompletion(db, zInfo(), zIH, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = pc.Close()
	if body, ok := zServeB(t, dataDir, zFCM(dataDir)); ok {
		from, to := zZeroRun(body, zWantB())
		t.Errorf("legacy row: cache path serves b, zeroes at [%d,%d)", from, to)
	}

	// A row that names its pieces, one of which is not complete (it failed a
	// hash since). Nothing has evicted this torrent under the new code, so
	// the eviction gate does not turn the request away; the pieces do.
	zExec(t, dataDir, `insert or replace into file_completion("path", first_piece, last_piece) values('pack/b.mkv', 0, 1)`)
	if body, ok := zServeB(t, dataDir, zFCM(dataDir)); ok {
		from, to := zZeroRun(body, zWantB())
		t.Errorf("row over an incomplete piece: cache path serves b, zeroes at [%d,%d)", from, to)
	}
}

// zPod is a seeder pod on dataDir's node with mi's torrent loaded: a torrent
// client whose storage has the given cache budget, and the web seeder in
// front of it. ts is the pod's storage of the torrent.
func zPod(tb testing.TB, dataDir string, mi *metainfo.MetaInfo, budget int64, stallTimeout time.Duration) (ws *WebSeeder, ts *mmapTorrentStorage) {
	tb.Helper()
	torrentsDir := tb.TempDir()
	f, err := os.Create(filepath.Join(torrentsDir, "pack.torrent"))
	if err != nil {
		tb.Fatal(err)
	}
	if err := mi.Write(f); err != nil {
		tb.Fatal(err)
	}
	_ = f.Close()
	tc := &TorrentClient{
		swarm:                 newSwarmStats(),
		rLimit:                -1,
		maxUnverifiedBytes:    -1,
		dataDir:               dataDir,
		perTorrentCacheBudget: budget,
		testConfig:            func(cfg *torrent.ClientConfig) { loopbackConfig(cfg) },
	}
	tb.Cleanup(tc.Close)
	tm := NewTorrentMap(tc, nil, &FileStoreMap{p: torrentsDir})
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String(DataDirFlag, dataDir, "")
	ws = &WebSeeder{tm: tm, fcm: zFCM(dataDir), tom: NewTouchMap(cli.NewContext(nil, fs, nil)), maxReadahead: 1 << 20, stallTimeout: stallTimeout}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tor, err := tm.Get(ctx, mi.HashInfoBytes().HexString())
	if err != nil || tor == nil {
		tb.Fatalf("load torrent: %v", err)
	}
	return ws, tor.Piece(0).Storage().PieceImpl.(mmapStoragePiece).t
}

// heldWriter is a client reading slowly: its first Write waits until
// unblocked, then everything is recorded.
type heldWriter struct {
	*httptest.ResponseRecorder
	once             sync.Once
	writing, unblock chan struct{}
}

func (w *heldWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.writing) })
	<-w.unblock
	return w.ResponseRecorder.Write(p)
}

// TestCachePathLeavesEvictingTorrentsToTheTorrent: a cache-path stream holds
// the torrent's dir for its whole response, and one that keeps moving is
// never cut (watchStall cuts only a stalled one). In production such streams
// ran up to 11 h, 172 h a day over 79 node+hash pairs (2026-10-03..04), and
// every eviction of the torrent on the node waited for them, the serving
// pod's own included. A torrent that is evicted is left to the torrent path:
// its reads go through the evicted-piece guard and touch the LRU, and the
// lock its storage holds does not stand in the way of its own evictions.
func TestCachePathLeavesEvictingTorrentsToTheTorrent(t *testing.T) {
	dataDir := zSeed(t)
	info := zInfo()
	// Eviction on, production's stall timeout: the stream below is within it.
	ws, ts := zPod(t, dataDir, zMI, 3*zpl-1, 10*time.Minute)

	w := &heldWriter{ResponseRecorder: httptest.NewRecorder(), writing: make(chan struct{}), unblock: make(chan struct{})}
	served := make(chan struct{})
	go func() {
		defer close(served)
		ws.serveFile(w, httptest.NewRequest(http.MethodGet, "/"+zHash+"/pack/b.mkv", nil), zHash, "pack/b.mkv")
	}()
	select {
	case <-w.writing:
	case <-time.After(10 * time.Second):
		close(w.unblock)
		t.Fatal("b was not served")
	}

	// c's piece arrives on the pod while b streams; the cache is over budget.
	p := info.Piece(2)
	sp := ts.Piece(p)
	if _, err := sp.WriteAt(zData()[p.Offset():p.Offset()+p.Length()], 0); err != nil {
		t.Fatal(err)
	}
	if err := sp.MarkComplete(); err != nil {
		t.Fatal(err)
	}
	if used := ts.lru.Used(); used > ts.lru.budget {
		t.Errorf("the pod's eviction waited for the stream of b: %d bytes cached over a budget of %d", used, ts.lru.budget)
	}

	close(w.unblock)
	<-served
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), zWantB()) {
		t.Errorf("status %d, %d bytes; b's bytes: %v", w.Code, w.Body.Len(), bytes.Equal(w.Body.Bytes(), zWantB()))
	}
}
