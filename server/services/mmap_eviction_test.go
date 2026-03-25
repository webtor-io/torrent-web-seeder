package services

import (
	"crypto/sha1"
	"os"
	"testing"

	"github.com/anacrolix/torrent/metainfo"
)

// makeDummyPieces creates a Pieces byte slice with n dummy 20-byte hashes.
func makeDummyPieces(n int) []byte {
	pieces := make([]byte, n*sha1.Size)
	for i := range pieces {
		pieces[i] = byte(i % 256)
	}
	return pieces
}

func TestPieceFileRegions_SingleFile(t *testing.T) {
	info := &metainfo.Info{
		PieceLength: 100,
		Length:      500,
		Name:        "test",
		Pieces:      makeDummyPieces(5),
	}
	ts := &mmapTorrentStorage{
		info:     info,
		fileLens: []int64{500},
	}

	// Piece 0: offset=0, length=100 → file 0, offset 0, length 100.
	regions := ts.pieceFileRegions(info.Piece(0))
	if len(regions) != 1 {
		t.Fatalf("expected 1 region, got %d", len(regions))
	}
	if regions[0].fileIndex != 0 || regions[0].offset != 0 || regions[0].length != 100 {
		t.Fatalf("unexpected region: %+v", regions[0])
	}

	// Piece 2: offset=200, length=100 → file 0, offset 200, length 100.
	regions = ts.pieceFileRegions(info.Piece(2))
	if len(regions) != 1 {
		t.Fatalf("expected 1 region, got %d", len(regions))
	}
	if regions[0].offset != 200 || regions[0].length != 100 {
		t.Fatalf("unexpected region: %+v", regions[0])
	}
}

func TestPieceFileRegions_MultiFile(t *testing.T) {
	// Two files: 150 bytes and 150 bytes. Piece length 100.
	// Total = 300 bytes = 3 pieces.
	// Piece 0: 0-100 → entirely in file 0.
	// Piece 1: 100-200 → 50 in file 0 (offset 100) + 50 in file 1 (offset 0).
	// Piece 2: 200-300 → entirely in file 1 (offset 50).
	info := &metainfo.Info{
		PieceLength: 100,
		Name:        "test",
		Pieces:      makeDummyPieces(3),
		Files: []metainfo.FileInfo{
			{Length: 150, Path: []string{"a.txt"}},
			{Length: 150, Path: []string{"b.txt"}},
		},
	}
	ts := &mmapTorrentStorage{
		info:     info,
		fileLens: []int64{150, 150},
	}

	// Piece 0: should be in file 0 only.
	regions := ts.pieceFileRegions(info.Piece(0))
	if len(regions) != 1 {
		t.Fatalf("piece 0: expected 1 region, got %d", len(regions))
	}
	if regions[0].fileIndex != 0 || regions[0].offset != 0 || regions[0].length != 100 {
		t.Fatalf("piece 0: unexpected region: %+v", regions[0])
	}

	// Piece 1: should span file 0 and file 1.
	regions = ts.pieceFileRegions(info.Piece(1))
	if len(regions) != 2 {
		t.Fatalf("piece 1: expected 2 regions, got %d: %+v", len(regions), regions)
	}
	if regions[0].fileIndex != 0 || regions[0].offset != 100 || regions[0].length != 50 {
		t.Fatalf("piece 1 region 0: expected {0, 100, 50}, got %+v", regions[0])
	}
	if regions[1].fileIndex != 1 || regions[1].offset != 0 || regions[1].length != 50 {
		t.Fatalf("piece 1 region 1: expected {1, 0, 50}, got %+v", regions[1])
	}

	// Piece 2: should be in file 1 only, offset 50, length 100.
	regions = ts.pieceFileRegions(info.Piece(2))
	if len(regions) != 1 {
		t.Fatalf("piece 2: expected 1 region, got %d", len(regions))
	}
	if regions[0].fileIndex != 1 || regions[0].offset != 50 || regions[0].length != 100 {
		t.Fatalf("piece 2: unexpected region: %+v", regions[0])
	}
}

func TestPieceFileRegions_LastPieceShorter(t *testing.T) {
	// File of 250 bytes, piece length 100.
	// Piece 0: 0-100, Piece 1: 100-200, Piece 2: 200-250 (only 50 bytes).
	info := &metainfo.Info{
		PieceLength: 100,
		Length:      250,
		Name:        "test",
		Pieces:      makeDummyPieces(3),
	}
	ts := &mmapTorrentStorage{
		info:     info,
		fileLens: []int64{250},
	}

	lastPiece := info.Piece(2)
	if lastPiece.Length() != 50 {
		t.Fatalf("expected last piece length 50, got %d", lastPiece.Length())
	}
	regions := ts.pieceFileRegions(lastPiece)
	if len(regions) != 1 {
		t.Fatalf("expected 1 region, got %d", len(regions))
	}
	if regions[0].offset != 200 || regions[0].length != 50 {
		t.Fatalf("unexpected region: %+v", regions[0])
	}
}

func TestCompletions_Uncomplete(t *testing.T) {
	info := &metainfo.Info{
		PieceLength: 100,
		Name:        "test",
		Pieces:      makeDummyPieces(3),
		Files: []metainfo.FileInfo{
			{Length: 200, Path: []string{"a.txt"}},
			{Length: 100, Path: []string{"b.txt"}},
		},
	}
	c := &completions{
		pieces:         make([]bool, 3),
		completedCount: 0,
		completedFiles: make(map[string]bool),
		info:           info,
	}

	// Complete all pieces.
	c.Complete(0)
	c.Complete(1)
	c.Complete(2)
	if c.completedCount != 3 {
		t.Fatalf("expected 3 completed, got %d", c.completedCount)
	}
	if !c.completed {
		t.Fatal("expected completed=true")
	}

	// Mark file a.txt as complete in the cache.
	c.completedFiles["test/a.txt"] = true

	// Uncomplete piece 1 — it belongs to file a.txt (pieces 0-2 cover 0-300).
	affected := c.Uncomplete(1)
	if c.completedCount != 2 {
		t.Fatalf("expected 2 completed, got %d", c.completedCount)
	}
	if c.completed {
		t.Fatal("expected completed=false after uncomplete")
	}
	if c.pieces[1] {
		t.Fatal("piece 1 should be false")
	}

	// Should have removed a.txt from completedFiles.
	if c.completedFiles["test/a.txt"] {
		t.Fatal("expected test/a.txt removed from completedFiles")
	}
	if len(affected) == 0 {
		t.Fatal("expected affected files list")
	}
	found := false
	for _, f := range affected {
		if f == "test/a.txt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected test/a.txt in affected, got %v", affected)
	}
}

func TestCompletions_Uncomplete_AlreadyIncomplete(t *testing.T) {
	info := &metainfo.Info{
		PieceLength: 100,
		Length:      200,
		Name:        "test",
		Pieces:      makeDummyPieces(2),
	}
	c := &completions{
		pieces:         make([]bool, 2),
		completedCount: 0,
		completedFiles: make(map[string]bool),
		info:           info,
	}

	// Uncomplete a piece that was never complete — should be a no-op.
	affected := c.Uncomplete(0)
	if c.completedCount != 0 {
		t.Fatalf("expected 0, got %d", c.completedCount)
	}
	if len(affected) != 0 {
		t.Fatalf("expected no affected files, got %v", affected)
	}
}

func TestCompletions_Complete_Idempotent(t *testing.T) {
	info := &metainfo.Info{
		PieceLength: 100,
		Length:      200,
		Name:        "test",
		Pieces:      makeDummyPieces(2),
	}
	c := &completions{
		pieces:         make([]bool, 2),
		completedCount: 0,
		completedFiles: make(map[string]bool),
		info:           info,
	}

	c.Complete(0)
	c.Complete(0) // duplicate
	if c.completedCount != 1 {
		t.Fatalf("expected 1 (idempotent), got %d", c.completedCount)
	}
}

func TestHolePunch(t *testing.T) {
	// Test the hole punch implementation (on macOS: zero-fill stub, on Linux: fallocate).
	f, err := createTempFileWithData(t, 1024, 0xAB)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// Punch a hole at offset 100, length 200.
	err = punchHole(f, 100, 200)
	if err != nil {
		t.Fatalf("punchHole failed: %v", err)
	}

	// Read back and verify zeros in the punched region.
	buf := make([]byte, 200)
	_, err = f.ReadAt(buf, 100)
	if err != nil {
		t.Fatal(err)
	}
	for i, b := range buf {
		if b != 0 {
			t.Fatalf("expected zero at offset %d, got %d", 100+i, b)
		}
	}

	// Verify data outside the hole is unchanged.
	before := make([]byte, 100)
	_, _ = f.ReadAt(before, 0)
	for i, b := range before {
		if b != 0xAB {
			t.Fatalf("expected 0xAB at offset %d, got %d", i, b)
		}
	}

	after := make([]byte, 100)
	_, _ = f.ReadAt(after, 300)
	for i, b := range after {
		if b != 0xAB {
			t.Fatalf("expected 0xAB at offset %d, got %d", 300+i, b)
		}
	}
}

func createTempFileWithData(t *testing.T, size int, fillByte byte) (*os.File, error) {
	t.Helper()
	f, err := os.CreateTemp("", "hole_punch_test")
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() {
		os.Remove(f.Name())
	})
	data := make([]byte, size)
	for i := range data {
		data[i] = fillByte
	}
	_, err = f.Write(data)
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
