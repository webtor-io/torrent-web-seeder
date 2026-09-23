package services

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

// A torrent none of whose pieces were ever downloaded gets a persistent
// completion and is not hashed when it is added. A piece that fails the
// library's initial check is marked not complete, which writes it a
// completion row and turns its completion Ok; with the check skipped, no
// piece has one. Eight pieces hash in microseconds, so the test looks for
// that trace rather than for the Checking state, which is gone before it
// could be seen.
func TestAddTorrent_SkipsInitialPieceCheck(t *testing.T) {
	const pieceLen = 16 << 10
	const numPieces = 8
	infoBytes, err := bencode.Marshal(metainfo.Info{
		Name:        "never-downloaded.bin",
		PieceLength: pieceLen,
		Pieces:      makeDummyPieces(numPieces),
		Length:      pieceLen * numPieces,
	})
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = dir
	cfg.DefaultStorage = NewMMap(dir, 0, FileCacheConfig{})
	cfg.ListenPort = 0
	cfg.NoDHT = true
	cfg.DisableTrackers = true
	cfg.DisableUTP = true
	cfg.DisableIPv6 = true
	cfg.DisableWebtorrent = true
	cfg.DisableWebseeds = true
	cfg.NoDefaultPortForwarding = true
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	tor, err := addTorrent(cl, &metainfo.MetaInfo{InfoBytes: infoBytes})
	if err != nil {
		t.Fatal(err)
	}
	if tor.NumPieces() != numPieces {
		t.Fatalf("NumPieces = %d, want %d", tor.NumPieces(), numPieces)
	}
	// The completion must be the persistent one. Its db sits in the
	// torrent's dir, which did not exist on a first open, and the in-memory
	// fallback loses everything the session downloads.
	db := filepath.Join(dir, tor.InfoHash().HexString(), ".torrent.db")
	if _, err := os.Stat(db); err != nil {
		t.Fatalf("no completion db after add: %v", err)
	}
	// Let the hashers drain whatever was queued, then look for its trace.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		checking := false
		for _, r := range tor.PieceStateRuns() {
			checking = checking || r.Checking
		}
		if !checking {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	for i := 0; i < numPieces; i++ {
		if ps := tor.PieceState(i); ps.Ok {
			t.Fatalf("piece %d has a completion row (complete=%v): it was hashed on add", i, ps.Complete)
		}
	}
}
