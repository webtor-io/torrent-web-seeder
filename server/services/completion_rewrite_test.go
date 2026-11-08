package services

import (
	"testing"

	"github.com/go-llsqlite/adapter"
	"github.com/go-llsqlite/adapter/sqlitex"
)

// dbChanges is total_changes() of pc's connection: rows written since open,
// an `insert or replace` of an identical row included.
func dbChanges(t *testing.T, pc *pieceCompletion) (n int) {
	t.Helper()
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if err := sqlitex.Exec(pc.db, `select total_changes()`, func(s *sqlite.Stmt) error {
		n = s.ColumnInt(0)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return
}

// fileRow reports whether path has a file_completion row and whether its
// piece columns are filled.
func fileRow(t *testing.T, pc *pieceCompletion, path string) (present, pieces bool) {
	t.Helper()
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if err := sqlitex.Exec(pc.db, `select first_piece is not null and last_piece is not null from file_completion where "path"=?`,
		func(s *sqlite.Stmt) error {
			present, pieces = true, s.ColumnInt(0) == 1
			return nil
		}, path); err != nil {
		t.Fatal(err)
	}
	return
}

// The completion loop offers every complete file every 5 s until the whole
// torrent is complete; only the first offer per completion may write.
func TestCompleteFileWritesOncePerCompletion(t *testing.T) {
	pc := newEventsPC(t, nil)
	const f = "pack/sub/b.mkv"

	start := dbChanges(t, pc)
	if err := pc.CompleteFile(f); err != nil {
		t.Fatal(err)
	}
	written := dbChanges(t, pc)
	if written != start+1 {
		t.Fatalf("first completion: %d rows written, want 1", written-start)
	}
	for tick := 0; tick < 3; tick++ {
		if err := pc.CompleteFile(f); err != nil {
			t.Fatal(err)
		}
	}
	if got := dbChanges(t, pc); got != written {
		t.Fatalf("3 more ticks rewrote %d rows, want 0", got-written)
	}

	// Evicted: the row goes, and the next completion writes it again.
	publish, err := pc.UncompleteFiles([]string{f})
	if err != nil {
		t.Fatal(err)
	}
	publish()
	if present, _ := fileRow(t, pc, f); present {
		t.Fatal("row survived eviction")
	}
	if err := pc.CompleteFile(f); err != nil {
		t.Fatal(err)
	}
	if present, pieces := fileRow(t, pc, f); !present || !pieces {
		t.Fatalf("re-completed after eviction: present=%v pieces=%v, want true/true", present, pieces)
	}
}

// A row from before first_piece/last_piece has them NULL and is not served
// from cache; the first offer of a load rewrites it.
func TestCompleteFileFillsOldRowOnLoad(t *testing.T) {
	pc := newEventsPC(t, nil)
	const f = "pack/a.mkv"
	pc.mu.Lock()
	err := sqlitex.Exec(pc.db, `insert into file_completion("path") values(?)`, nil, f)
	pc.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := pc.CompleteFile(f); err != nil {
		t.Fatal(err)
	}
	if present, pieces := fileRow(t, pc, f); !present || !pieces {
		t.Fatalf("old row after first offer: present=%v pieces=%v, want true/true", present, pieces)
	}
}
