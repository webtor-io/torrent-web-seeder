package services

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"golang.org/x/time/rate"
)

// The block for one torrent keeps its peer lines, which follow blank lines
// inside the block, and nothing of the torrent listed next to it.
func TestTorrentStatusBlock_OneTorrentWithItsPeers(t *testing.T) {
	data, mi := addTestPayload(t)
	// A seeder that uploads at 16 KiB/s keeps the 132 KB download, and so
	// the connection, alive for seconds; a complete torrent drops it.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "payload.bin"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	scfg := addTestConfig(dir)
	scfg.Seed = true
	scfg.UploadRateLimiter = rate.NewLimiter(16<<10, 16<<10)
	st := storage.NewFileOpts(storage.NewFileClientOpts{
		ClientBaseDir:   dir,
		PieceCompletion: storage.NewMapPieceCompletion(),
		UsePartFiles:    g.Some(false),
	})
	scfg.DefaultStorage = st
	seeder, err := torrent.NewClient(scfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { seeder.Close(); _ = st.Close() })
	if _, err := seeder.AddTorrent(mi); err != nil {
		t.Fatal(err)
	}

	cl := mmapClient(t, t.TempDir(), false)
	tor, err := addTorrent(cl, mi)
	if err != nil {
		t.Fatal(err)
	}
	tor.AddClientPeer(seeder)
	r := tor.NewReader()
	t.Cleanup(func() { _ = r.Close() })
	go func() { _, _ = io.Copy(io.Discard, r) }()
	deadline := time.Now().Add(10 * time.Second)
	for len(tor.PeerConns()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no connection to the seeding client")
		}
		time.Sleep(10 * time.Millisecond)
	}

	otherInfo, err := bencode.Marshal(metainfo.Info{
		Name:        "other.bin",
		PieceLength: 16 << 10,
		Pieces:      makeDummyPieces(2),
		Length:      32 << 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	other, err := addTorrent(cl, &metainfo.MetaInfo{InfoBytes: otherInfo})
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	cl.WriteStatus(&buf)
	block := torrentStatusBlock(buf.String(), tor.InfoHash().HexString())
	if !strings.Contains(block, "Infohash: "+tor.InfoHash().HexString()) {
		t.Fatalf("block lacks its infohash:\n%s", block)
	}
	if want := fmt.Sprintf(":%d", seeder.LocalPort()); !strings.Contains(block, want) {
		t.Errorf("block lacks the peer at %s:\n%s", want, block)
	}
	if strings.Contains(block, other.InfoHash().HexString()) {
		t.Errorf("block runs into the other torrent:\n%s", block)
	}
	if got := torrentStatusBlock(buf.String(), strings.Repeat("0", 40)); got != "" {
		t.Errorf("unknown hash gave a block:\n%s", got)
	}
}
