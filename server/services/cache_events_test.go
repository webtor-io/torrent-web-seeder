package services

import (
	"os"
	"sync"
	"testing"

	"github.com/anacrolix/torrent/metainfo"
)

type recPublisher struct {
	mu   sync.Mutex
	msgs []string
}

func (r *recPublisher) Publish(subject string, data []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, subject+" "+string(data))
	return nil
}

func (r *recPublisher) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.msgs
	r.msgs = nil
	return out
}

func eventsInfo() *metainfo.Info {
	return &metainfo.Info{
		Name:        "pack",
		PieceLength: 4,
		Pieces:      make([]byte, 20*4),
		Files: []metainfo.FileInfo{
			{Path: []string{"a.mkv"}, Length: 8},
			{Path: []string{"sub", "b.mkv"}, Length: 8},
		},
	}
}

func newEventsPC(t *testing.T, rec *recPublisher) *pieceCompletion {
	t.Helper()
	dir, err := os.MkdirTemp("", "pc-events")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	var ev *CacheEvents
	if rec != nil {
		ev = &CacheEvents{p: rec}
	}
	pc, err := NewPieceCompletion(dir, eventsInfo(), metainfo.Hash{0xab}, ev)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	return pc
}

const evHash = "ab00000000000000000000000000000000000000"

func TestCacheEventsTransitions(t *testing.T) {
	rec := &recPublisher{}
	pc := newEventsPC(t, rec)

	// The completion loop calls CompleteFile for every complete file on every
	// 5 s tick: the event is the transition, once.
	for i := 0; i < 3; i++ {
		if err := pc.CompleteFile("pack/sub/b.mkv"); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := rec.take(), []string{`resource.cached {"resource_id":"` + evHash + `","file_idx":1}`}; !equalStrings(got, want) {
		t.Fatalf("cached: got %v, want %v", got, want)
	}

	// Eviction of a piece of a file never announced says nothing -- the
	// single-file-over-budget case, one call per evicted piece.
	if err := pc.UncompleteFiles([]string{"pack/a.mkv"}); err != nil {
		t.Fatal(err)
	}
	if got := rec.take(); len(got) != 0 {
		t.Fatalf("never announced: got %v", got)
	}

	// The announced one is taken back, once.
	for i := 0; i < 2; i++ {
		if err := pc.UncompleteFiles([]string{"pack/sub/b.mkv"}); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := rec.take(), []string{`resource.uncached {"resource_id":"` + evHash + `","file_idx":1}`}; !equalStrings(got, want) {
		t.Fatalf("uncached: got %v, want %v", got, want)
	}

	// Re-downloaded after eviction: announced again.
	if err := pc.CompleteFile("pack/sub/b.mkv"); err != nil {
		t.Fatal(err)
	}
	if got := rec.take(); len(got) != 1 {
		t.Fatalf("re-completed: got %v", got)
	}
}

func TestCacheEventsOffAndUnknownPath(t *testing.T) {
	// No NATS: nil events, every call is a no-op rather than a nil deref.
	pc := newEventsPC(t, nil)
	if err := pc.CompleteFile("pack/a.mkv"); err != nil {
		t.Fatal(err)
	}
	if err := pc.UncompleteFiles([]string{"pack/a.mkv"}); err != nil {
		t.Fatal(err)
	}
	var off *CacheEvents
	off.Cached(evHash, 0)
	off.Close()

	// A path that is not a file of this torrent has no index; nothing valid
	// can be said about it.
	rec := &recPublisher{}
	pc = newEventsPC(t, rec)
	if err := pc.CompleteFile("pack/nope.mkv"); err != nil {
		t.Fatal(err)
	}
	if got := rec.take(); len(got) != 0 {
		t.Fatalf("unknown path: got %v", got)
	}
}

func TestFileIndexesSingleFile(t *testing.T) {
	m := fileIndexes(&metainfo.Info{Name: "movie.mkv", Length: 10})
	if len(m) != 1 || m["movie.mkv"] != 0 {
		t.Fatalf("got %v", m)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
