package services

import (
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"github.com/go-llsqlite/adapter/sqlitex"
)

// A piece with no row is known and not complete; the library never requests
// a piece whose completion is unknown. Only a failed query is unknown.
func TestPieceCompletion_Get(t *testing.T) {
	info := &metainfo.Info{Name: "f", PieceLength: 4, Length: 12, Pieces: makeDummyPieces(3)}
	hash := metainfo.Hash{0xcd}
	pc, err := NewPieceCompletion(t.TempDir(), info, hash, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	if err := pc.Set(metainfo.PieceKey{InfoHash: hash, Index: 1}, true); err != nil {
		t.Fatal(err)
	}
	if err := pc.Set(metainfo.PieceKey{InfoHash: hash, Index: 2}, false); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		index int
		want  storage.Completion
	}{
		{"no row", 0, storage.Completion{Ok: true, Complete: false}},
		{"complete=1", 1, storage.Completion{Ok: true, Complete: true}},
		{"complete=0", 2, storage.Completion{Ok: true, Complete: false}},
	} {
		got, err := pc.Get(metainfo.PieceKey{InfoHash: hash, Index: tc.index})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: Get = %+v, want %+v", tc.name, got, tc.want)
		}
	}

	// A db that cannot answer is not "never downloaded".
	if err := sqlitex.ExecScript(pc.db, `drop table piece_completion`); err != nil {
		t.Fatal(err)
	}
	got, err := pc.Get(metainfo.PieceKey{InfoHash: hash, Index: 0})
	if err == nil || got.Ok {
		t.Fatalf("Get on a failing db = %+v, %v; want Ok=false and an error", got, err)
	}
}

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
