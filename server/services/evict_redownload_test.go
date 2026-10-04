package services

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

// The evicted flag clears on the first chunk written after an eviction. That
// alone does not let a responsive reader see the piece's other, still empty
// chunks: the library admits a reader only to chunks it has marked written
// (dirty) or to a complete piece. It does when the piece is evicted AGAIN
// while it is being downloaded: the punch erases chunks already written and
// marked dirty, the next chunk clears the flag, and the reader takes the
// hole for data. Production does evict one piece several times in a row —
// ltshr 2026-10-02 07:34:18, "evicted piece 32254, freed 0 bytes" three
// times within a second: eviction lists computed by concurrent
// MarkCompletes (5 hashers) and the sweep overlap, and evictPiece punches
// whatever it is given.
//
// Both readers serveFile makes are checked: a player's (responsive) and a
// download's, which waits for hashed pieces. The waiting one hung on an
// evicted piece before evictPiece told the library it was gone (105705c).
func TestEvictedPieceRedownload_ReaderNeverSeesHoles(t *testing.T) {
	for _, evictAgain := range []bool{false, true} {
		for _, responsive := range []bool{true, false} {
			name := "evicted once"
			if evictAgain {
				name = "evicted again mid-download"
			}
			name += map[bool]string{true: "/player", false: "/download"}[responsive]
			t.Run(name, func(t *testing.T) { testEvictedPieceRedownload(t, evictAgain, responsive) })
		}
	}
}

func testEvictedPieceRedownload(t *testing.T, evictAgain, responsive bool) {
	const (
		pieceLen = 64 << 10
		pieces   = 8
		p        = 1
	)
	data, mi := multiChunkPayload(t, pieceLen, pieces)
	seeder := seedingClient(t, data, mi)
	want := data[p*pieceLen : (p+1)*pieceLen]

	// Eviction is on (the torrent is larger than the budget), but nothing is
	// evicted by itself: only three pieces are ever read.
	dir := t.TempDir()
	st := NewMMap(dir, pieces*pieceLen-1, FileCacheConfig{})
	hook := &evictHook{ClientImpl: st, piece: p, failed: make(chan struct{}), release: make(chan struct{})}
	cfg := addTestConfig(dir)
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

	if got := readPiece(t, tor, p, pieceLen, responsive, 20*time.Second); !bytes.Equal(got, want) {
		t.Fatal("first download of the piece returned other bytes")
	}
	// A responsive read returns before the hash; wait for it.
	for deadline := time.Now().Add(10 * time.Second); !tor.PieceState(p).Complete; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("piece not complete after the first read")
		}
	}

	// The eviction the LRU asked for.
	hook.ts.evictPiece(p)
	if tor.PieceState(p).Complete {
		t.Fatal("piece still complete after eviction")
	}
	hook.mu.Lock()
	hook.armed, hook.evictAgain = true, evictAgain
	hook.mu.Unlock()

	if !evictAgain {
		// Re-downloaded while a responsive reader waits on it.
		if got := readPiece(t, tor, p, pieceLen, responsive, 20*time.Second); !bytes.Equal(got, want) {
			t.Fatalf("re-downloaded piece read with zeroed chunks %v", zeroChunks(got, want))
		}
		return
	}

	// Download the piece without a reader; the hook evicts it again after
	// its first chunk is written. Then read it the way serveFile does.
	// The piece either fails its hash (the second punch erased a written
	// chunk; MarkNotComplete is held there) or, if a second eviction cannot
	// touch it, completes.
	tor.Piece(p).SetPriority(torrent.PiecePriorityNow)
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		select {
		case <-hook.failed:
		default:
			if !tor.PieceState(p).Complete {
				if time.Now().After(deadline) {
					t.Fatalf("the re-downloaded piece neither completed nor failed; piece runs %v", tor.PieceStateRuns())
				}
				continue
			}
		}
		break
	}
	got := readPieceWithin(tor, p, pieceLen, responsive, 3*time.Second)
	hook.unblock()
	if len(got) == len(want) && !bytes.Equal(got, want) {
		t.Fatalf("the reader got the piece with zeroed chunks %v (first chunk written before the second eviction: %d)",
			zeroChunks(got, want), hook.firstChunk/(16<<10))
	}
}

// readPiece reads piece i through a file reader set up as serveFile sets it.
func readPiece(t *testing.T, tor *torrent.Torrent, i, pieceLen int, responsive bool, d time.Duration) []byte {
	t.Helper()
	got := readPieceWithin(tor, i, pieceLen, responsive, d)
	if len(got) != pieceLen {
		t.Fatalf("read %d of %d bytes of piece %d in %v; piece runs %v", len(got), pieceLen, i, d, tor.PieceStateRuns())
	}
	return got
}

func readPieceWithin(tor *torrent.Torrent, i, pieceLen int, responsive bool, d time.Duration) []byte {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	r := tor.Files()[0].NewReader()
	defer r.Close()
	r.SetContext(ctx)
	if responsive {
		r.SetResponsive()
	}
	r.SetReadaheadFunc(NewReadaheadFunc(1 << 20))
	if _, err := r.Seek(int64(i*pieceLen), io.SeekStart); err != nil {
		return nil
	}
	b := make([]byte, pieceLen)
	n, _ := io.ReadFull(r, b)
	return b[:n]
}

// evictHook wraps the service's storage: it exposes the torrent storage so
// the test can evict, evicts piece `piece` once more right after its first
// chunk write once armed (when evictAgain), and holds that piece's first
// MarkNotComplete until released.
type evictHook struct {
	storage.ClientImpl
	ts      *mmapTorrentStorage
	piece   int
	failed  chan struct{}
	release chan struct{}
	relOnce sync.Once
	nmc     sync.Once

	mu         sync.Mutex
	armed      bool
	evictAgain bool
	evicted    bool
	firstChunk int64
}

func (h *evictHook) unblock() { h.relOnce.Do(func() { close(h.release) }) }

func (h *evictHook) OpenTorrent(ctx context.Context, info *metainfo.Info, ih metainfo.Hash) (storage.TorrentImpl, error) {
	ti, err := h.ClientImpl.OpenTorrent(ctx, info, ih)
	if err != nil {
		return ti, err
	}
	h.ts = ti.Piece(info.Piece(0)).(mmapStoragePiece).t
	inner := ti.Piece
	ti.Piece = func(p metainfo.Piece) storage.PieceImpl {
		return evictHookPiece{PieceImpl: inner(p), index: p.Index(), h: h}
	}
	return ti, nil
}

type evictHookPiece struct {
	storage.PieceImpl
	index int
	h     *evictHook
}

func (p evictHookPiece) WriteAt(b []byte, off int64) (int, error) {
	n, err := p.PieceImpl.WriteAt(b, off)
	if p.index != p.h.piece {
		return n, err
	}
	p.h.mu.Lock()
	again := p.h.armed && p.h.evictAgain && !p.h.evicted
	if again {
		p.h.evicted, p.h.firstChunk = true, off
	}
	p.h.mu.Unlock()
	if again {
		// A stale eviction list reaching this piece: the library has the
		// client lock released around storage writes, as evictPiece needs.
		p.h.ts.evictPiece(p.index)
	}
	return n, err
}

func (p evictHookPiece) MarkNotComplete() error {
	if p.index == p.h.piece {
		p.h.nmc.Do(func() {
			close(p.h.failed)
			select {
			case <-p.h.release:
			case <-time.After(30 * time.Second):
			}
		})
	}
	return p.PieceImpl.MarkNotComplete()
}

// evicted[] is pod-local, piece_completion is node-wide. Pod A evicts piece
// p while alone. Pod B loads the torrent afterwards and downloads p:
// piece_completion[p] = 1. A's library then re-reads p's completion from
// storage -- its reader does that for every piece of its readahead window
// after any read error (fork reader.go:386-388); Piece.UpdateCompletion
// stands in for it here. A takes B's 1 and reads B's bytes from the shared
// disk. Keeping its flag, it answered every read of p with ErrPieceEvicted for
// as long as it held the torrent; calling p incomplete instead, it downloaded
// p again and wrote over the bytes B had verified.
func TestForeignCompletionOverLocalEvictedFlag(t *testing.T) {
	t.Run("B re-downloads", func(t *testing.T) { testForeignCompletion(t, true) })
	t.Run("control: nobody re-downloads", func(t *testing.T) { testForeignCompletion(t, false) })
}

func testForeignCompletion(t *testing.T, bDownloads bool) {
	const (
		pieceLen = 64 << 10
		pieces   = 8
		p        = 1
	)
	data, mi := multiChunkPayload(t, pieceLen, pieces)
	seeder := seedingClient(t, data, mi)
	want := data[p*pieceLen : (p+1)*pieceLen]
	dir := t.TempDir()

	stA := NewMMap(dir, pieces*pieceLen-1, FileCacheConfig{})
	hook := &evictHook{ClientImpl: stA, piece: -1, failed: make(chan struct{}), release: make(chan struct{})}
	cfgA := addTestConfig(t.TempDir())
	cfgA.DefaultStorage = hook
	clA, err := torrent.NewClient(cfgA)
	if err != nil {
		t.Fatal(err)
	}
	stA.SetClient(clA)
	t.Cleanup(func() { clA.Close() })
	torA, err := clA.AddTorrent(mi)
	if err != nil {
		t.Fatal(err)
	}
	torA.AddClientPeer(seeder)
	if got := readPiece(t, torA, p, pieceLen, false, 20*time.Second); !bytes.Equal(got, want) {
		t.Fatal("pod A first download differs")
	}
	// Everything the read at the end reaches (p and on, within the budget),
	// and nothing wanted after: that read can then download nothing but p.
	torA.DownloadPieces(p, pieces)
	for i, deadline := p, time.Now().Add(20*time.Second); i < pieces; {
		if torA.PieceState(i).Complete {
			i++
		} else if time.Now().After(deadline) {
			t.Fatalf("pod A did not download piece %d", i)
		} else {
			time.Sleep(10 * time.Millisecond)
		}
	}
	torA.CancelPieces(p, pieces)
	if !hook.ts.evictPiece(p) || torA.PieceState(p).Complete {
		t.Fatal("pod A could not evict the piece while alone")
	}

	if bDownloads {
		stB := NewMMap(dir, 0, FileCacheConfig{})
		cfgB := addTestConfig(t.TempDir())
		cfgB.DefaultStorage = stB
		clB, err := torrent.NewClient(cfgB)
		if err != nil {
			t.Fatal(err)
		}
		stB.SetClient(clB)
		t.Cleanup(func() { clB.Close() })
		torB, err := clB.AddTorrent(mi)
		if err != nil {
			t.Fatal(err)
		}
		torB.AddClientPeer(seeder)
		if got := readPiece(t, torB, p, pieceLen, false, 20*time.Second); !bytes.Equal(got, want) {
			t.Fatal("pod B download differs")
		}
	}

	stats := torA.Stats()
	before := stats.ChunksReadUseful.Int64()
	torA.Piece(p).UpdateCompletion()
	if got := readPieceWithin(torA, p, pieceLen, false, 5*time.Second); !bytes.Equal(got, want) {
		t.Errorf("pod A reads %d of %d bytes of piece %d (A complete: %v, A evicted flag: %v)",
			len(got), pieceLen, p, torA.PieceState(p).Complete, hook.ts.isEvicted(p))
	}
	stats = torA.Stats()
	chunks := stats.ChunksReadUseful.Int64() - before
	if bDownloads && chunks != 0 {
		t.Errorf("pod A downloaded %d chunks over piece %d, which pod B had verified", chunks, p)
	}
	if !bDownloads && chunks == 0 {
		t.Error("control: pod A read its evicted piece without downloading it")
	}
}

// Completion clears the flag only for a piece the db calls complete, and
// reads the two as one step against a punch of this pod's. Between the two, a
// punch sets the db to 0 and the flag; Completion would hold the 1 it read
// before, take the flag for one left by an earlier punch, and clear it over
// the hole just punched.
func TestCompletionKeepsTheFlagOfAHole(t *testing.T) {
	impl := zOpen(t, zSeed(t), nil)
	t.Cleanup(func() { _ = impl.Close() })
	ts := zStorage(impl)
	punched := make(chan struct{})
	var once sync.Once
	ts.pc = getHookPC{ts.pc, func() {
		once.Do(func() {
			go func() { ts.evictPiece(1); close(punched) }()
			select {
			case <-punched:
			case <-time.After(200 * time.Millisecond): // blocked by the shard lock
			}
		})
	}}
	impl.Piece(zInfo().Piece(1)).Completion()
	<-punched
	if !ts.isEvicted(1) {
		t.Fatal("punched under Completion: piece 1 is a hole and its evicted flag is clear")
	}
	impl.Piece(zInfo().Piece(1)).Completion()
	if !ts.isEvicted(1) {
		t.Error("Completion of the incomplete piece 1 cleared its evicted flag")
	}
}

// getHookPC runs afterGet after each Get.
type getHookPC struct {
	storage.PieceCompletion
	afterGet func()
}

func (p getHookPC) Get(pk metainfo.PieceKey) (storage.Completion, error) {
	c, err := p.PieceCompletion.Get(pk)
	p.afterGet()
	return c, err
}

// Another pod's MarkNotComplete sets the db to 0 under a piece this pod still
// has in its LRU, so this pod downloads it again; a punch of its own lands
// after the hash has read the piece and before MarkComplete. A 1 set over that
// hole would read, in Completion, as another pod's verification: the flag goes
// and the piece reads as zeroes with no error (reader.go:341 resyncs
// completion after ErrPieceEvicted, so the zeroes reach HTTP and Vault).
// "punch during MarkComplete": the shard lock keeps a punch from falling
// between MarkComplete's check of the flag and its Set.
func TestMarkCompleteOverAHole(t *testing.T) {
	t.Run("punch before MarkComplete", func(t *testing.T) { testMarkCompleteOverAHole(t, false) })
	t.Run("punch during MarkComplete", func(t *testing.T) { testMarkCompleteOverAHole(t, true) })
}

func testMarkCompleteOverAHole(t *testing.T, during bool) {
	impl := zOpen(t, zSeed(t), nil)
	t.Cleanup(func() { _ = impl.Close() })
	ts := zStorage(impl)
	p := zInfo().Piece(1)
	sp := impl.Piece(p)
	if err := ts.pc.Set(metainfo.PieceKey{InfoHash: zIH, Index: 1}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := sp.WriteAt(zData()[p.Offset():p.Offset()+p.Length()], 0); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, p.Length())
	if _, err := sp.ReadAt(buf, 0); err != nil { // the hash
		t.Fatal(err)
	}
	punched := make(chan struct{})
	punch := func() {
		if !ts.evictPiece(1) {
			t.Error("could not evict piece 1 while alone")
		}
		close(punched)
	}
	if during {
		ts.pc = setHookPC{ts.pc, func() {
			go punch()
			select {
			case <-punched:
			case <-time.After(200 * time.Millisecond): // blocked by the shard lock
			}
		}}
	} else {
		punch()
	}
	_ = sp.MarkComplete()
	<-punched
	c := sp.Completion()
	n, err := sp.ReadAt(buf, 0)
	if c.Complete || !errors.Is(err, ErrPieceEvicted) {
		t.Errorf("punched piece 1: Completion %+v, ReadAt n=%d err=%v, zeroes %v",
			c, n, err, bytes.Count(buf[:n], []byte{0}) == n && n > 0)
	}
}

// setHookPC runs beforeSet before a Set of true.
type setHookPC struct {
	storage.PieceCompletion
	beforeSet func()
}

func (p setHookPC) Set(pk metainfo.PieceKey, b bool) error {
	if b {
		p.beforeSet()
	}
	return p.PieceCompletion.Set(pk, b)
}
