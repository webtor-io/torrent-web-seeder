package services

import (
	"bytes"
	"context"
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
