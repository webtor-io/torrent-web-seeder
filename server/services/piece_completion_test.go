package services

import (
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
)

// The completion loop reads the all-complete flag that Set writes as pieces
// complete, and it read it without the lock: an end-to-end download under
// -race tripped over it about once in 80 runs. This test orders the two
// accesses the way that shows it every time, and says nothing without -race.
func TestPieceCompletion_LoopReadsCompletedUnderLock(t *testing.T) {
	info := &metainfo.Info{Name: "f", PieceLength: 4, Length: 8, Pieces: makeDummyPieces(2)}
	hash := metainfo.Hash{0xce}
	pc, err := NewPieceCompletion(t.TempDir(), info, hash, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	// The loop's first pass reads the flag at once and then sleeps 5 s:
	// write it after that read.
	time.Sleep(50 * time.Millisecond)
	for i := 0; i < 2; i++ {
		if err := pc.Set(metainfo.PieceKey{InfoHash: hash, Index: i}, true); err != nil {
			t.Fatal(err)
		}
	}
}
