package services

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

// Storage level. Pod B's hash of piece 1 fails (MarkNotComplete, db=0)
// while piece 1 sits complete in pod A's LRU. A's library resyncs piece 1
// (Completion: incomplete) and downloads it again; its first chunk is written.
// B lets go of the torrent. A's next MarkComplete (piece 2) evicts the LRU's
// oldest idle piece, piece 1: lru.Has(1) is true, so the punch erases the
// chunk A has just written. A's next chunk clears the flag, and the erased
// chunk, which the library holds as written, reads as zeroes with err nil.
func TestForeignNotCompleteLeavesTheLRU(t *testing.T) {
	dataDir := zSeed(t)
	info := zInfo()
	implA := zOpen(t, dataDir, nil)
	t.Cleanup(func() { _ = implA.Close() })
	tsA := zStorage(implA)
	zTouch(t, implA, info, 0)
	zTouch(t, implA, info, 3)
	if !tsA.lru.Has(1) {
		t.Fatal("piece 1 not in A's LRU")
	}

	implB, err := NewMMap(dataDir, 0, FileCacheConfig{}).OpenTorrent(context.Background(), info, zIH)
	if err != nil {
		t.Fatal(err)
	}
	if err := implB.Piece(info.Piece(1)).MarkNotComplete(); err != nil { // B's hash of piece 1 failed
		t.Fatal(err)
	}

	p1 := implA.Piece(info.Piece(1))
	if p1.Completion().Complete { // A's reader resyncs its window
		t.Fatal("A still sees piece 1 complete")
	}
	data := zData()[info.Piece(1).Offset() : info.Piece(1).Offset()+info.Piece(1).Length()]
	const half = zpl / 2
	if _, err := p1.WriteAt(data[:half], 0); err != nil { // A's first chunk of the re-download
		t.Fatal(err)
	}
	_ = implB.Close()

	zWritePiece(t, implA, info, 2) // MarkComplete(2): over budget, the LRU evicts piece 1
	if tsA.lru.Has(1) {
		t.Logf("piece 1 still in LRU after MarkComplete(2)")
	}
	if _, err := p1.WriteAt(data[half:], half); err != nil { // A's next chunk
		t.Fatal(err)
	}
	buf := make([]byte, half)
	n, err := p1.ReadAt(buf, 0) // a responsive reader on the chunk the library has as written
	if err == nil && n == half && bytes.Count(buf, []byte{0}) == n {
		t.Errorf("A reads its written chunk of piece 1 as %d zero bytes with err nil (evicted flag %v, db complete %v)",
			n, tsA.isEvicted(1), p1.Completion().Complete)
	} else {
		t.Logf("read n=%d err=%v zeros=%d", n, err, bytes.Count(buf[:n], []byte{0}))
	}
}

// Library level. Same as TestForeignNotCompleteLeavesTheLRU through anacrolix: the player's
// (responsive) reader gets zeroed chunks of piece p; the download's reader
// waits for the hash (control).
func TestForeignNotCompleteMidRedownload(t *testing.T) {
	for _, responsive := range []bool{true, false} {
		name := map[bool]string{true: "player", false: "download"}[responsive]
		t.Run(name, func(t *testing.T) { testForeignNotCompleteMidRedownload(t, responsive) })
	}
}

func testForeignNotCompleteMidRedownload(t *testing.T, responsive bool) {
	const (
		pieceLen = 64 << 10
		pieces   = 8
		p        = 1
	)
	data, mi := multiChunkPayload(t, pieceLen, pieces)
	seeder := seedingClient(t, data, mi)
	want := data[p*pieceLen : (p+1)*pieceLen]
	info, err := mi.UnmarshalInfo()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	st := NewMMap(dir, pieces*pieceLen-1, FileCacheConfig{})
	hook := &evictHook{ClientImpl: st, piece: p, failed: make(chan struct{}), release: make(chan struct{})}
	cfg := addTestConfig(t.TempDir())
	cfg.DefaultStorage = hook
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	st.SetClient(cl)
	t.Cleanup(func() { cl.Close() })
	t.Cleanup(hook.unblock)
	tor, err := cl.AddTorrent(mi)
	if err != nil {
		t.Fatal(err)
	}
	tor.AddClientPeer(seeder)
	if got := readPiece(t, tor, p, pieceLen, false, 20*time.Second); !bytes.Equal(got, want) {
		t.Fatal("first download differs")
	}
	if !hook.ts.lru.Has(p) {
		t.Fatal("piece not in A's LRU")
	}

	// Pod B holds the torrent; its hash of p fails.
	implB, err := NewMMap(dir, 0, FileCacheConfig{}).OpenTorrent(context.Background(), &info, mi.HashInfoBytes())
	if err != nil {
		t.Fatal(err)
	}
	if err := implB.Piece(info.Piece(p)).MarkNotComplete(); err != nil {
		t.Fatal(err)
	}
	// A's reader resyncs its window (fork reader.go:386-388).
	tor.Piece(p).UpdateCompletion()
	if tor.PieceState(p).Complete {
		t.Fatal("A still complete after resync")
	}
	_ = implB.Close()

	// The LRU's eviction of p reaches it after the re-download's first chunk.
	hook.mu.Lock()
	hook.armed, hook.evictAgain = true, true
	hook.mu.Unlock()
	tor.Piece(p).SetPriority(torrent.PiecePriorityNow)
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		select {
		case <-hook.failed:
		default:
			if !tor.PieceState(p).Complete {
				if time.Now().After(deadline) {
					t.Fatalf("neither completed nor failed")
				}
				continue
			}
		}
		break
	}
	got := readPieceWithin(tor, p, pieceLen, responsive, 3*time.Second)
	hook.unblock()
	if len(got) == len(want) && !bytes.Equal(got, want) {
		t.Fatalf("%s reader got piece %d with zeroed chunks %v (first chunk at %d, evicted %v)",
			map[bool]string{true: "player", false: "download"}[responsive], p, zeroChunks(got, want), hook.firstChunk, hook.evicted)
	}
	t.Logf("read %d bytes, evicted again %v", len(got), hook.evicted)
}

// Library level, MarkComplete refusing a punched piece. After the same resync, A's punch lands
// between the re-download's hash and MarkComplete: MarkComplete refuses, the
// library calls the piece complete anyway. Both readers must get the real
// bytes (after another download), never zeroes.
func TestRefusedMarkCompleteReaders(t *testing.T) {
	for _, responsive := range []bool{true, false} {
		name := map[bool]string{true: "player", false: "download"}[responsive]
		t.Run(name, func(t *testing.T) { testRefusedMarkCompleteReaders(t, responsive) })
	}
}

type mcHook struct {
	storage.ClientImpl
	ts      *mmapTorrentStorage
	piece   int
	armed   chan struct{}
	refused chan error
}

func (h *mcHook) OpenTorrent(ctx context.Context, info *metainfo.Info, ih metainfo.Hash) (storage.TorrentImpl, error) {
	ti, err := h.ClientImpl.OpenTorrent(ctx, info, ih)
	if err != nil {
		return ti, err
	}
	h.ts = ti.Piece(info.Piece(0)).(mmapStoragePiece).t
	inner := ti.Piece
	ti.Piece = func(p metainfo.Piece) storage.PieceImpl {
		return mcHookPiece{PieceImpl: inner(p), index: p.Index(), h: h}
	}
	return ti, nil
}

type mcHookPiece struct {
	storage.PieceImpl
	index int
	h     *mcHook
}

func (p mcHookPiece) MarkComplete() error {
	if p.index == p.h.piece {
		select {
		case <-p.h.armed:
			p.h.ts.evictPiece(p.index) // punch between the hash and MarkComplete
			err := p.PieceImpl.MarkComplete()
			select {
			case p.h.refused <- err:
			default:
			}
			return err
		default:
		}
	}
	return p.PieceImpl.MarkComplete()
}

func testRefusedMarkCompleteReaders(t *testing.T, responsive bool) {
	const (
		pieceLen = 64 << 10
		pieces   = 8
		p        = 1
	)
	data, mi := multiChunkPayload(t, pieceLen, pieces)
	seeder := seedingClient(t, data, mi)
	want := data[p*pieceLen : (p+1)*pieceLen]
	info, err := mi.UnmarshalInfo()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	st := NewMMap(dir, pieces*pieceLen-1, FileCacheConfig{})
	hook := &mcHook{ClientImpl: st, piece: p, armed: make(chan struct{}), refused: make(chan error, 1)}
	cfg := addTestConfig(t.TempDir())
	cfg.DefaultStorage = hook
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	st.SetClient(cl)
	t.Cleanup(func() { cl.Close() })
	tor, err := cl.AddTorrent(mi)
	if err != nil {
		t.Fatal(err)
	}
	tor.AddClientPeer(seeder)
	if got := readPiece(t, tor, p, pieceLen, false, 20*time.Second); !bytes.Equal(got, want) {
		t.Fatal("first download differs")
	}
	implB, err := NewMMap(dir, 0, FileCacheConfig{}).OpenTorrent(context.Background(), &info, mi.HashInfoBytes())
	if err != nil {
		t.Fatal(err)
	}
	if err := implB.Piece(info.Piece(p)).MarkNotComplete(); err != nil {
		t.Fatal(err)
	}
	tor.Piece(p).UpdateCompletion()
	_ = implB.Close()
	close(hook.armed)
	tor.Piece(p).SetPriority(torrent.PiecePriorityNow)
	select {
	case err := <-hook.refused:
		t.Logf("MarkComplete after the punch: %v; library complete %v, flag %v", err, tor.PieceState(p).Complete, hook.ts.isEvicted(p))
	case <-time.After(20 * time.Second):
		t.Fatal("no MarkComplete of the re-download")
	}
	got := readPieceWithin(tor, p, pieceLen, responsive, 20*time.Second)
	if len(got) > 0 && !bytes.Equal(got, want[:len(got)]) {
		t.Fatalf("reader got %d bytes with zeroed chunks %v", len(got), zeroChunks(got, want[:len(got)]))
	}
	if len(got) != pieceLen {
		t.Errorf("reader got %d of %d bytes (piece stuck?) complete=%v flag=%v", len(got), pieceLen, tor.PieceState(p).Complete, hook.ts.isEvicted(p))
	}
}
