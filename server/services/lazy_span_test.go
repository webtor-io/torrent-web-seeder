package services

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"github.com/edsrzf/mmap-go"
)

// spanFixture builds a span over files of the given lengths under a temp dir
// with the given cache config and returns it with a reference buffer of the
// same length filled with a byte pattern that encodes the span offset.
func spanFixture(t *testing.T, cfg FileCacheConfig, lens ...int64) (*lazySpan, []byte) {
	t.Helper()
	dir := t.TempDir()
	files := make([]spanFile, len(lens))
	for i, l := range lens {
		files[i] = spanFile{path: filepath.Join(dir, "content", fmt.Sprintf("%02x", i%256), fmt.Sprintf("f%d", i)), length: l}
	}
	s := newLazySpan(files, cfg)
	t.Cleanup(func() { _ = s.Close() })
	ref := make([]byte, s.Len())
	for i := range ref {
		ref[i] = byte(i*7 + i/251)
	}
	return s, ref
}

// The reference layout used by most tests: small files (pread path), a
// zero-length file, files straddling piece boundaries, one big mapped file.
var mixedLens = []int64{10, 0, 300, 5, 4096, 70000, 1, 123456}

func TestLazySpan_Layout(t *testing.T) {
	s, _ := spanFixture(t, FileCacheConfig{}, mixedLens...)
	var want int64
	for i, l := range mixedLens {
		off, length := s.fileRange(i)
		if off != want || length != l {
			t.Errorf("file %d: range = (%d,%d), want (%d,%d)", i, off, length, want, l)
		}
		want += l
	}
	if s.Len() != want {
		t.Errorf("Len = %d, want %d", s.Len(), want)
	}
	if s.openCount() != 0 {
		t.Errorf("a new span must hold no open files, got %d", s.openCount())
	}
	if entries, _ := os.ReadDir(filepath.Dir(filepath.Dir(s.files[0].path))); len(entries) != 0 {
		t.Errorf("a new span must create nothing on disk, found %d entries", len(entries))
	}
}

// locate must agree with the straightforward linear scan on random
// layouts and ranges, never emit zero-length files, and cover the range
// exactly.
func TestLazySpan_LocateMatchesLinearScan(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for iter := 0; iter < 300; iter++ {
		n := 1 + rng.Intn(12)
		lens := make([]int64, n)
		for i := range lens {
			switch rng.Intn(4) {
			case 0:
				lens[i] = 0
			default:
				lens[i] = int64(rng.Intn(5000))
			}
		}
		s := newLazySpan(filesFor(t, lens), FileCacheConfig{})
		for q := 0; q < 20; q++ {
			off := int64(rng.Intn(int(s.Len()) + 100))
			length := int64(rng.Intn(6000))
			got := s.locate(off, length)
			want := linearLocate(lens, off, length)
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("lens=%v off=%d len=%d\n got  %v\n want %v", lens, off, length, got, want)
			}
			var covered int64
			for _, r := range got {
				if r.length <= 0 || lens[r.fileIndex] == 0 {
					t.Fatalf("bad region %+v for lens=%v", r, lens)
				}
				covered += r.length
			}
			if exp := max(0, min(off+length, s.Len())-min(off, s.Len())); covered != exp {
				t.Fatalf("covered %d, want %d (lens=%v off=%d len=%d)", covered, exp, lens, off, length)
			}
		}
	}
}

func filesFor(t *testing.T, lens []int64) []spanFile {
	t.Helper()
	dir := t.TempDir()
	files := make([]spanFile, len(lens))
	for i, l := range lens {
		files[i] = spanFile{path: filepath.Join(dir, fmt.Sprintf("f%d", i)), length: l}
	}
	return files
}

// linearLocate is the eager span's O(files) scan, kept as the oracle.
func linearLocate(lens []int64, off, length int64) []fileRegion {
	var regions []fileRegion
	var fileOff int64
	end := off + length
	for i, l := range lens {
		fEnd := fileOff + l
		if l > 0 && end > fileOff && off < fEnd {
			start := max(off, fileOff) - fileOff
			stop := min(end, fEnd) - fileOff
			if stop > start {
				regions = append(regions, fileRegion{fileIndex: i, offset: start, length: stop - start})
			}
		}
		fileOff = fEnd
	}
	return regions
}

// Bytes written through the span come back through the span and through
// the files on disk, across every file boundary, for both the mapped and the
// pread paths; a read past the end is short with io.EOF.
func TestLazySpan_RoundTrip(t *testing.T) {
	s, ref := spanFixture(t, FileCacheConfig{MmapMin: 1024}, mixedLens...)

	// Write in odd-sized chunks so writes straddle file boundaries.
	for off := int64(0); off < s.Len(); off += 777 {
		end := min(off+777, s.Len())
		if n, err := s.WriteAt(ref[off:end], off); err != nil || int64(n) != end-off {
			t.Fatalf("WriteAt(%d): n=%d err=%v", off, n, err)
		}
	}

	got := make([]byte, s.Len())
	for off := int64(0); off < s.Len(); off += 1000 {
		end := min(off+1000, s.Len())
		if n, err := s.ReadAt(got[off:end], off); err != nil || int64(n) != end-off {
			t.Fatalf("ReadAt(%d): n=%d err=%v", off, n, err)
		}
	}
	if !bytes.Equal(got, ref) {
		t.Fatal("span read back differs from what was written")
	}

	// On disk, file by file.
	var off int64
	for i, sf := range s.files {
		data, err := os.ReadFile(sf.path)
		if sf.length == 0 {
			if err == nil {
				t.Errorf("file %d has zero length and must never be created", i)
			}
			continue
		}
		if err != nil {
			t.Fatalf("file %d: %v", i, err)
		}
		if !bytes.Equal(data, ref[off:off+sf.length]) {
			t.Errorf("file %d content differs", i)
		}
		off += sf.length
	}

	// A read crossing the end is short and says so.
	buf := make([]byte, 100)
	n, err := s.ReadAt(buf, s.Len()-40)
	if n != 40 || err != io.EOF {
		t.Errorf("read past end: n=%d err=%v, want 40, io.EOF", n, err)
	}
	if n, err := s.ReadAt(buf, s.Len()+5); n != 0 || err != io.EOF {
		t.Errorf("read beyond end: n=%d err=%v, want 0, io.EOF", n, err)
	}
	if n, err := s.WriteAt(buf, s.Len()-10); n != 10 || err != io.ErrShortWrite {
		t.Errorf("write past end: n=%d err=%v, want 10, ErrShortWrite", n, err)
	}
}

// io.SectionReader over the span, the way Piece() uses it, reads a piece
// that straddles several files.
func TestLazySpan_SectionReaderPiece(t *testing.T) {
	s, ref := spanFixture(t, FileCacheConfig{MmapMin: 1024}, mixedLens...)
	if _, err := s.WriteAt(ref, 0); err != nil {
		t.Fatal(err)
	}
	const pieceLen = 4000
	for off := int64(0); off < s.Len(); off += pieceLen {
		length := min(pieceLen, s.Len()-off)
		sr := io.NewSectionReader(s, off, length)
		got, err := io.ReadAll(sr)
		if err != nil {
			t.Fatalf("piece at %d: %v", off, err)
		}
		if !bytes.Equal(got, ref[off:off+length]) {
			t.Fatalf("piece at %d differs", off)
		}
	}
}

// Only touched files open, and only files at or above MmapMin get a mapping.
func TestLazySpan_OpensOnTouchAndMapsBySize(t *testing.T) {
	s, _ := spanFixture(t, FileCacheConfig{MmapMin: 1024}, mixedLens...)
	buf := make([]byte, 4)
	// Touch file 2 (300 bytes, pread) and file 5 (70000, mapped) only.
	off2, _ := s.fileRange(2)
	off5, _ := s.fileRange(5)
	for _, off := range []int64{off2 + 10, off5 + 100} {
		if _, err := s.ReadAt(buf, off); err != nil {
			t.Fatal(err)
		}
	}
	if s.openCount() != 2 {
		t.Fatalf("open = %d, want 2", s.openCount())
	}
	s.mu.Lock()
	e2, e5 := s.open[2], s.open[5]
	s.mu.Unlock()
	if e2 == nil || e2.m != nil {
		t.Errorf("file 2 (300 B) must be open without a mapping, got %+v", e2)
	}
	if e5 == nil || e5.m == nil || len(e5.m) != 70000 {
		t.Errorf("file 5 (70000 B) must be open and mapped, got %+v", e5)
	}
	for i, sf := range s.files {
		_, err := os.Stat(sf.path)
		if i == 2 || i == 5 {
			if err != nil {
				t.Errorf("file %d must exist on disk", i)
			}
		} else if err == nil {
			t.Errorf("file %d was never touched and must not exist", i)
		}
	}
}

// The cache never holds more idle files than MaxOpen; evicted files reopen
// transparently with their data intact.
func TestLazySpan_MaxOpenBoundAndReopen(t *testing.T) {
	lens := make([]int64, 20)
	for i := range lens {
		lens[i] = 100
	}
	s, ref := spanFixture(t, FileCacheConfig{MaxOpen: 3, MmapMin: 50}, lens...)
	if _, err := s.WriteAt(ref, 0); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 10)
	for i := range lens {
		off, _ := s.fileRange(i)
		if _, err := s.ReadAt(buf, off); err != nil {
			t.Fatal(err)
		}
		if c := s.openCount(); c > 3 {
			t.Fatalf("after touching %d files: open = %d, want <= 3", i+1, c)
		}
	}
	// The three most recent survive; the first is closed and reopens on read.
	s.mu.Lock()
	_, first := s.open[0]
	_, last := s.open[19]
	s.mu.Unlock()
	if first || !last {
		t.Errorf("LRU order wrong: file0 open=%v, file19 open=%v", first, last)
	}
	got := make([]byte, s.Len())
	if _, err := s.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, ref) {
		t.Fatal("data lost across close/reopen")
	}
}

// A file that is in use is never closed, even when that leaves the table
// over the cap; it is closed once released and another open pushes it out.
func TestLazySpan_InUseEntryIsNotEvicted(t *testing.T) {
	lens := []int64{100, 100, 100, 100}
	s, _ := spanFixture(t, FileCacheConfig{MaxOpen: 1, MmapMin: 50}, lens...)
	s.closeMu.RLock()
	held, err := s.acquire(0)
	s.closeMu.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 10)
	for i := 1; i < 4; i++ {
		off, _ := s.fileRange(i)
		if _, err := s.ReadAt(buf, off); err != nil {
			t.Fatal(err)
		}
	}
	s.mu.Lock()
	_, stillOpen := s.open[0]
	c := len(s.open)
	s.mu.Unlock()
	if !stillOpen || held.f == nil || held.m == nil {
		t.Fatal("an acquired entry was evicted or closed under its holder")
	}
	if c != 2 {
		t.Errorf("open = %d, want 2 (the held one plus the cap of 1)", c)
	}
	s.release(held)
	off3, _ := s.fileRange(3)
	if _, err := s.ReadAt(buf, off3); err != nil {
		t.Fatal(err)
	}
	// Touch a fourth file: the released entry (now the oldest idle) goes.
	off2, _ := s.fileRange(2)
	if _, err := s.ReadAt(buf, off2); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	_, stillOpen = s.open[0]
	s.mu.Unlock()
	if stillOpen {
		t.Error("released entry must be evictable")
	}
}

// Many readers over many files with a tiny cache: every byte still comes back
// right and nothing crashes on an unmap. Run with -race.
func TestLazySpan_ConcurrentReadersSmallCache(t *testing.T) {
	lens := make([]int64, 64)
	for i := range lens {
		lens[i] = int64(2000 + (i*977)%9000)
	}
	s, ref := spanFixture(t, FileCacheConfig{MaxOpen: 4, MmapMin: 3000}, lens...)
	if _, err := s.WriteAt(ref, 0); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var bad atomic.Int64
	deadline := time.Now().Add(400 * time.Millisecond)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			buf := make([]byte, 5000)
			for time.Now().Before(deadline) {
				off := int64(rng.Intn(int(s.Len()) - 5000))
				n, err := s.ReadAt(buf, off)
				if err != nil || n != len(buf) || !bytes.Equal(buf, ref[off:off+5000]) {
					bad.Add(1)
				}
			}
		}(int64(g))
	}
	wg.Wait()
	if bad.Load() != 0 {
		t.Fatalf("%d bad reads", bad.Load())
	}
	if c := s.openCount(); c > 4 {
		t.Errorf("open = %d after readers finished, want <= 4", c)
	}
}

// Close waits for an in-flight read, then everything is closed and further
// use is an error rather than a crash.
func TestLazySpan_CloseWaitsForReadersThenRefuses(t *testing.T) {
	s, ref := spanFixture(t, FileCacheConfig{MmapMin: 50}, 5000, 5000)
	if _, err := s.WriteAt(ref, 0); err != nil {
		t.Fatal(err)
	}
	s.closeMu.RLock() // simulate a reader in flight
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case <-closed:
		t.Fatal("Close returned while a reader held the span")
	case <-time.After(50 * time.Millisecond):
	}
	s.closeMu.RUnlock()
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if s.openCount() != 0 {
		t.Errorf("open = %d after Close", s.openCount())
	}
	buf := make([]byte, 10)
	if _, err := s.ReadAt(buf, 0); !errors.Is(err, ErrSpanClosed) {
		t.Errorf("ReadAt after Close: %v, want ErrSpanClosed", err)
	}
	if _, err := s.WriteAt(buf, 0); !errors.Is(err, ErrSpanClosed) {
		t.Errorf("WriteAt after Close: %v, want ErrSpanClosed", err)
	}
	if err := s.withFile(0, func(*os.File, mmap.MMap) error { return nil }); !errors.Is(err, ErrSpanClosed) {
		t.Errorf("withFile after Close: %v, want ErrSpanClosed", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// withFile opens a cold file for hole punching and hands over the mapping
// only when the file is mapped; ifMapped never opens anything.
func TestLazySpan_WithFileAndIfMapped(t *testing.T) {
	s, _ := spanFixture(t, FileCacheConfig{MmapMin: 1024}, 100, 70000)
	called := 0
	s.ifMapped(1, func(mmap.MMap) { called++ })
	if called != 0 || s.openCount() != 0 {
		t.Fatal("ifMapped must not open a cold file")
	}
	err := s.withFile(1, func(f *os.File, m mmap.MMap) error {
		if f == nil || m == nil {
			return fmt.Errorf("mapped file: f=%v m=%v", f != nil, m != nil)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = s.withFile(0, func(f *os.File, m mmap.MMap) error {
		if f == nil || m != nil {
			return fmt.Errorf("small file: f=%v m=%v", f != nil, m != nil)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s.ifMapped(1, func(m mmap.MMap) { called++ })
	if called != 1 {
		t.Errorf("ifMapped on a mapped file must run, called=%d", called)
	}
}

// torrentSpanFiles keeps the content/<2hex>/<sha1> layout and the torrent's
// file order and lengths, and opens nothing.
func TestTorrentSpanFiles_LayoutAndOrder(t *testing.T) {
	info := &metainfo.Info{
		Name:        "show",
		PieceLength: 1024,
		Pieces:      makeDummyPieces(1),
		Files: []metainfo.FileInfo{
			{Path: []string{"a", "one.mkv"}, Length: 300},
			{Path: []string{"b", "empty"}, Length: 0},
			{Path: []string{"c.txt"}, Length: 7},
		},
	}
	dir := t.TempDir()
	files, err := torrentSpanFiles(info, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 || files[0].length != 300 || files[1].length != 0 || files[2].length != 7 {
		t.Fatalf("files = %+v", files)
	}
	for _, f := range files {
		rel, _ := filepath.Rel(dir, f.path)
		if len(rel) != len("content/xx/")+40 || rel[:8] != "content/" || rel[8:10] != rel[11:13] {
			t.Errorf("layout: %q", rel)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("torrentSpanFiles must not touch the disk, found %d entries", len(entries))
	}
}

// End to end through the storage: writing a piece, reading it, evicting it
// (hole punch through the span) and re-reading gives the eviction error,
// then a rewrite makes it readable again.
func TestMMapStorage_LazyEvictRoundTrip(t *testing.T) {
	const pieceLen = 4096
	info := &metainfo.Info{
		Name:        "lazy",
		PieceLength: pieceLen,
		Pieces:      makeDummyPieces(3),
		Files: []metainfo.FileInfo{
			{Path: []string{"small.h"}, Length: 100},
			{Path: []string{"big.bin"}, Length: 3*pieceLen - 100},
		},
	}
	var infoHash metainfo.Hash
	copy(infoHash[:], "lazyevict1234567890a")
	dir := t.TempDir()
	files, err := torrentSpanFiles(info, dir)
	if err != nil {
		t.Fatal(err)
	}
	span := newLazySpan(files, FileCacheConfig{MaxOpen: 1, MmapMin: 1024})
	ts := &mmapTorrentStorage{
		infoHash: infoHash,
		span:     span,
		pc:       storage.NewMapPieceCompletion(),
		lru:      NewPieceLRU(3 * pieceLen),
		info:     info,
		closeCh:  make(chan struct{}),
		evicted:  make([]atomic.Bool, 3),
	}
	defer ts.Close()

	data := bytes.Repeat([]byte{0xAB}, pieceLen)
	for i := 0; i < 3; i++ {
		p := ts.Piece(info.Piece(i))
		if _, err := p.WriteAt(data, 0); err != nil {
			t.Fatalf("write piece %d: %v", i, err)
		}
		if err := p.MarkComplete(); err != nil {
			t.Fatalf("mark %d: %v", i, err)
		}
	}
	buf := make([]byte, pieceLen)
	if _, err := ts.Piece(info.Piece(0)).ReadAt(buf, 0); err != nil || !bytes.Equal(buf, data) {
		t.Fatalf("read piece 0 before eviction: %v", err)
	}
	ts.evictPiece(0)
	if _, err := ts.Piece(info.Piece(0)).ReadAt(buf, 0); !errors.Is(err, ErrPieceEvicted) {
		t.Fatalf("read after eviction: %v, want ErrPieceEvicted", err)
	}
	// The hole is real: the bytes on disk are zero where piece 0 lived.
	onDisk, err := os.ReadFile(files[1].path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(onDisk[:pieceLen-100], make([]byte, pieceLen-100)) {
		t.Error("piece 0's bytes in big.bin were not punched")
	}
	if !bytes.Equal(onDisk[pieceLen-100:pieceLen-100+pieceLen], data) {
		t.Error("piece 1's bytes must be untouched by evicting piece 0")
	}
	if _, err := ts.Piece(info.Piece(0)).WriteAt(data, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Piece(info.Piece(0)).ReadAt(buf, 0); err != nil || !bytes.Equal(buf, data) {
		t.Fatalf("read after rewrite: %v", err)
	}
	if c := span.openCount(); c > 1 {
		t.Errorf("open = %d, want <= 1 (MaxOpen)", c)
	}
}
