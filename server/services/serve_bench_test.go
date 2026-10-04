package services

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	sqlite "github.com/go-llsqlite/adapter"
	"github.com/go-llsqlite/adapter/sqlitex"
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
	benchServe(b, &WebSeeder{fcm: NewFileCacheMap(c), tom: NewTouchMap(c)}, ih.HexString(), info.Name)
}

// BenchmarkServeFileEvicting is the same request for a complete 16 MiB file of
// a torrent with eviction on, loaded on the pod. The cache path used to answer
// it; it goes to the torrent now (FileCacheMap.Open).
func BenchmarkServeFileEvicting(b *testing.B) {
	const pl = 4 << 20
	info := &metainfo.Info{Name: "bench", PieceLength: pl, Pieces: makeDummyPieces(5), Files: []metainfo.FileInfo{
		{Path: []string{"a.bin"}, Length: 4 * pl},
		{Path: []string{"b.bin"}, Length: pl}, // not downloaded: the torrent is over the budget, its cache is not
	}}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		b.Fatal(err)
	}
	mi := &metainfo.MetaInfo{InfoBytes: infoBytes}
	ih := mi.HashInfoBytes()
	dataDir := b.TempDir()
	impl, err := NewMMap(dataDir, 0, FileCacheConfig{}).OpenTorrent(context.Background(), info, ih)
	if err != nil {
		b.Fatal(err)
	}
	buf := make([]byte, pl)
	for i := range buf {
		buf[i] = byte(i%251) + 1
	}
	for i := 0; i < 4; i++ {
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
	pc, err := NewPieceCompletion(filepath.Join(dataDir, ih.HexString()), info, ih, nil)
	if err != nil {
		b.Fatal(err)
	}
	if err := pc.CompleteFile("bench/a.bin"); err != nil {
		b.Fatal(err)
	}
	_ = pc.Close()
	ws, _ := zPod(b, dataDir, mi, 4*pl, 10*time.Minute)
	benchServe(b, ws, ih.HexString(), "bench/a.bin")
}

func benchServe(b *testing.B, s *WebSeeder, h, name string) {
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
			req := httptest.NewRequest(http.MethodGet, "/"+h+"/"+name, nil)
			if tc.rng != "" {
				req.Header.Set("Range", tc.rng)
			}
			b.SetBytes(int64(tc.size))
			for i := 0; i < b.N; i++ {
				w := httptest.NewRecorder()
				s.serveFile(w, req, h, name)
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

// BenchmarkIsDirComplete is the check behind ?stats, ?warmup and ?done on the
// root of a torrent of n one-piece files, all complete.
func BenchmarkIsDirComplete(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		dataDir, h := benchDirComplete(b, n)
		fcm := zFCM(dataDir)
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if ok, err := fcm.IsDirComplete(h, "", n); !ok || err != nil {
					b.Fatal(ok, err)
				}
			}
		})
	}
}

func benchDirComplete(b *testing.B, n int) (dataDir, h string) {
	b.Helper()
	const pl = 16 << 10
	info := &metainfo.Info{Name: "dir", PieceLength: pl, Pieces: makeDummyPieces(n)}
	for i := 0; i < n; i++ {
		info.Files = append(info.Files, metainfo.FileInfo{Path: []string{fmt.Sprintf("f%05d", i)}, Length: pl})
	}
	ih := metainfo.NewHashFromHex(fmt.Sprintf("d1c0%036x", n))
	dataDir, h = b.TempDir(), ih.HexString()
	dir := filepath.Join(dataDir, h)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		b.Fatal(err)
	}
	pc, err := NewPieceCompletion(dir, info, ih, nil) // the schema
	if err != nil {
		b.Fatal(err)
	}
	_ = pc.Close()
	db, err := sqlite.OpenConn(filepath.Join(dir, ".torrent.db"), 0)
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	exec := func(q string, args ...any) {
		if err := sqlitex.Exec(db, q, nil, args...); err != nil {
			b.Fatal(err)
		}
	}
	exec(`begin`)
	for i := 0; i < n; i++ {
		exec(`insert into piece_completion("index", complete) values(?, 1)`, i)
		exec(`insert into file_completion("path", first_piece, last_piece) values(?, ?, ?)`, fmt.Sprintf("dir/f%05d", i), i, i)
	}
	exec(`commit`)
	return dataDir, h
}
