package services

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/urfave/cli"
)

// benchCached is a node where the first n pieces of one 16 MiB file of four
// 4 MiB pieces are downloaded; with all four the file is recorded complete,
// as "serve file from cache" finds it.
func benchCached(b *testing.B, n int) (dataDir string, info *metainfo.Info, ih metainfo.Hash) {
	b.Helper()
	const pl = 4 << 20
	info = &metainfo.Info{Name: "bench.bin", PieceLength: pl, Pieces: makeDummyPieces(4), Length: 4 * pl}
	ih = metainfo.NewHashFromHex("be0c000000000000000000000000000000000000")
	dataDir = b.TempDir()
	impl, err := NewMMap(dataDir, 0, FileCacheConfig{}).OpenTorrent(context.Background(), info, ih)
	if err != nil {
		b.Fatal(err)
	}
	buf := make([]byte, pl)
	for i := range buf {
		buf[i] = byte(i%251) + 1
	}
	for i := 0; i < n; i++ {
		p := impl.Piece(info.Piece(i))
		if _, err := p.WriteAt(buf, 0); err != nil {
			b.Fatal(err)
		}
		if err := p.MarkComplete(); err != nil {
			b.Fatal(err)
		}
	}
	if err := impl.Close(); err != nil {
		b.Fatal(err)
	}
	if n < info.NumPieces() {
		return dataDir, info, ih
	}
	pc, err := NewPieceCompletion(filepath.Join(dataDir, ih.HexString()), info, ih, nil)
	if err != nil {
		b.Fatal(err)
	}
	if err := pc.CompleteFile(info.Name); err != nil {
		b.Fatal(err)
	}
	_ = pc.Close()
	return dataDir, info, ih
}

// BenchmarkServeFileFromCache is one request answered by the cache path of
// serveFile: a 1 MiB range, and the whole 16 MiB file.
func BenchmarkServeFileFromCache(b *testing.B) {
	dataDir, info, ih := benchCached(b, 4)
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	fs.String(DataDirFlag, dataDir, "")
	c := cli.NewContext(nil, fs, nil)
	s := &WebSeeder{fcm: NewFileCacheMap(c), tom: NewTouchMap(c)}
	h := ih.HexString()
	for _, tc := range []struct {
		name   string
		rng    string
		status int
		size   int
	}{
		{"range-1MiB", fmt.Sprintf("bytes=%d-%d", 5<<20, 6<<20-1), http.StatusPartialContent, 1 << 20},
		{"file-16MiB", "", http.StatusOK, 16 << 20},
	} {
		b.Run(tc.name, func(b *testing.B) {
			req := httptest.NewRequest(http.MethodGet, "/"+h+"/"+info.Name, nil)
			if tc.rng != "" {
				req.Header.Set("Range", tc.rng)
			}
			b.SetBytes(int64(tc.size))
			for i := 0; i < b.N; i++ {
				w := httptest.NewRecorder()
				s.serveFile(w, req, h, info.Name)
				if w.Code != tc.status || w.Body.Len() != tc.size {
					b.Fatalf("status %d, %d bytes", w.Code, w.Body.Len())
				}
			}
		})
	}
}

// BenchmarkStorageReadAt is the storage read under "serve file from torrent":
// 32 KiB reads of a complete piece of a torrent with eviction enabled (the
// budget is under the torrent's size and over what is on disk).
func BenchmarkStorageReadAt(b *testing.B) {
	dataDir, info, ih := benchCached(b, 3)
	impl, err := NewMMap(dataDir, info.TotalLength()-1, FileCacheConfig{}).OpenTorrent(context.Background(), info, ih)
	if err != nil {
		b.Fatal(err)
	}
	defer impl.Close()
	p := impl.Piece(info.Piece(2))
	buf := make([]byte, 32<<10)
	b.SetBytes(int64(len(buf)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		off := int64(i%128) * int64(len(buf))
		if _, err := p.ReadAt(buf, off); err != nil {
			b.Fatal(err)
		}
	}
}
