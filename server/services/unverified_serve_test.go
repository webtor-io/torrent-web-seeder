package services

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"github.com/urfave/cli"
)

// Vault stores what "serve file from torrent" hands it (it asks with
// download=true). A peer that sends bad bytes for a chunk (zeros, from a
// sparse or hole-punched copy it believes complete) is caught by the piece
// hash, but a download's reader must not have handed those bytes out before
// the hash ran.
//
// 2026-10-03, torrent 7bcdc68b, pod spdrw: the tail of a 619 KB file that
// shares its last piece with the next file was served with five 16 KiB
// chunks of zeros. Vault's two reads 4 s apart (hash pass and upload) got
// the same zeros — the stored key is sha256 of the zero-filled content — and
// the pod counted a failed piece hash two minutes later, once the next
// file's part of the piece had been downloaded.
//
// The test plays that: one peer holds a copy with a zeroed chunk and
// claims it complete (a seed-mode client). The pod's storage is the
// service's own; the request goes through WebSeeder.ServeHTTP. The only
// test-side hook holds the library's MarkNotComplete for a few seconds so
// the window between "chunk written" and "piece rejected" is as wide here
// as it was in production; a reader that waits for verified pieces gets
// nothing during the hold and then the good peer's bytes.
func TestServeFileFromTorrent_NeverServesUnverifiedChunks(t *testing.T) {
	const (
		pieceLen = 64 << 10 // four 16 KiB chunks
		pieces   = 4
		bad      = 1 // the piece whose chunk 1 the bad peer zeroes
	)
	data, mi := multiChunkPayload(t, pieceLen, pieces)
	ih := mi.HashInfoBytes()

	corrupt := bytes.Clone(data)
	clear(corrupt[bad*pieceLen+16<<10 : bad*pieceLen+32<<10])
	badPeer := seedModeClient(t, corrupt, mi)
	goodPeer := seedingClient(t, data, mi)

	dataDir := t.TempDir()
	hold := &holdHashFailure{piece: bad, failed: make(chan struct{}), release: make(chan struct{})}
	tc := &TorrentClient{
		swarm:              newSwarmStats(),
		rLimit:             -1,
		maxUnverifiedBytes: -1,
		dataDir:            dataDir,
		testConfig: func(cfg *torrent.ClientConfig) {
			loopbackConfig(cfg)
			hold.ClientImpl = cfg.DefaultStorage
			cfg.DefaultStorage = hold
		},
	}
	t.Cleanup(tc.Close)
	t.Cleanup(hold.unblock)

	torrentsDir := t.TempDir()
	tf, err := os.Create(filepath.Join(torrentsDir, "payload.torrent"))
	if err != nil {
		t.Fatal(err)
	}
	if err := mi.Write(tf); err != nil {
		t.Fatal(err)
	}
	_ = tf.Close()
	tm := NewTorrentMap(tc, nil, &FileStoreMap{p: torrentsDir})

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String(DataDirFlag, dataDir, "")
	c := cli.NewContext(nil, fs, nil)
	ws := &WebSeeder{tm: tm, fcm: NewFileCacheMap(c), tom: NewTouchMap(c), maxReadahead: 1 << 20}
	srv := httptest.NewServer(ws)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	tor, err := tm.Get(ctx, ih.HexString())
	if err != nil || tor == nil {
		t.Fatalf("load torrent: %v", err)
	}
	// Only the bad peer at first, so it is the one the piece comes from.
	tor.AddClientPeer(badPeer)

	type result struct {
		status int
		body   []byte
		err    error
	}
	done := make(chan result, 1)
	go func() {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
			fmt.Sprintf("%s/%s/payload.bin?download=true", srv.URL, ih.HexString()), nil)
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", bad*pieceLen, (bad+1)*pieceLen-1))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		done <- result{resp.StatusCode, b, err}
	}()

	select {
	case <-hold.failed:
	case <-time.After(30 * time.Second):
		t.Fatalf("piece %d never failed its hash; piece runs %v", bad, tor.PieceStateRuns())
	}
	var res result
	answered := false
	select {
	case res = <-done:
		answered = true
	case <-time.After(3 * time.Second):
	}
	hold.unblock()
	tor.AddClientPeer(goodPeer)
	if !answered {
		select {
		case res = <-done:
		case <-time.After(30 * time.Second):
			t.Fatalf("no response after the good peer joined; piece runs %v", tor.PieceStateRuns())
		}
	}
	if res.err != nil {
		t.Fatalf("GET: %v", res.err)
	}
	want := data[bad*pieceLen : (bad+1)*pieceLen]
	if res.status != http.StatusPartialContent || !bytes.Equal(res.body, want) {
		t.Fatalf("status %d, %d bytes; zeroed 16 KiB chunks of piece %d in the body: %v (answered before the piece was rejected: %v)",
			res.status, len(res.body), bad, zeroChunks(res.body, want), answered)
	}
}

// multiChunkPayload is random content of n pieces of pieceLen bytes in one
// file, with a v1 metainfo that hashes it.
func multiChunkPayload(t *testing.T, pieceLen, n int) ([]byte, *metainfo.MetaInfo) {
	t.Helper()
	data := make([]byte, pieceLen*n)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	var hashes []byte
	for off := 0; off < len(data); off += pieceLen {
		h := sha1.Sum(data[off : off+pieceLen])
		hashes = append(hashes, h[:]...)
	}
	infoBytes, err := bencode.Marshal(metainfo.Info{
		Name:        "payload.bin",
		PieceLength: int64(pieceLen),
		Pieces:      hashes,
		Length:      int64(len(data)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return data, &metainfo.MetaInfo{InfoBytes: infoBytes}
}

// seedModeClient seeds data without hashing it: its completion store says
// every piece is complete, as a client in seed mode (or one whose copy was
// hole-punched behind its back) believes.
func seedModeClient(t *testing.T, data []byte, mi *metainfo.MetaInfo) *torrent.Client {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "payload.bin"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := mi.UnmarshalInfo()
	if err != nil {
		t.Fatal(err)
	}
	pc := storage.NewMapPieceCompletion()
	for i := 0; i < info.NumPieces(); i++ {
		if err := pc.Set(metainfo.PieceKey{InfoHash: mi.HashInfoBytes(), Index: i}, true); err != nil {
			t.Fatal(err)
		}
	}
	st := storage.NewFileOpts(storage.NewFileClientOpts{
		ClientBaseDir:   dir,
		PieceCompletion: pc,
		UsePartFiles:    g.Some(false),
	})
	cfg := addTestConfig(dir)
	cfg.Seed = true
	cfg.DefaultStorage = st
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close(); _ = st.Close() })
	if _, err := cl.AddTorrent(mi); err != nil {
		t.Fatal(err)
	}
	return cl
}

// holdHashFailure wraps the pod's storage and holds the first
// MarkNotComplete of one piece until released (at most 30 s). The library
// calls it with the client lock released and before it un-dirties the
// piece's chunks, so the hold keeps the "written, not yet rejected" state.
type holdHashFailure struct {
	storage.ClientImpl
	piece   int
	once    sync.Once
	failed  chan struct{}
	release chan struct{}
	relOnce sync.Once
}

func (h *holdHashFailure) unblock() { h.relOnce.Do(func() { close(h.release) }) }

func (h *holdHashFailure) OpenTorrent(ctx context.Context, info *metainfo.Info, ih metainfo.Hash) (storage.TorrentImpl, error) {
	ti, err := h.ClientImpl.OpenTorrent(ctx, info, ih)
	if err != nil {
		return ti, err
	}
	inner := ti.Piece
	ti.Piece = func(p metainfo.Piece) storage.PieceImpl {
		return holdPiece{PieceImpl: inner(p), index: p.Index(), h: h}
	}
	return ti, nil
}

type holdPiece struct {
	storage.PieceImpl
	index int
	h     *holdHashFailure
}

func (p holdPiece) MarkNotComplete() error {
	if p.index == p.h.piece {
		p.h.once.Do(func() {
			close(p.h.failed)
			select {
			case <-p.h.release:
			case <-time.After(30 * time.Second):
			}
		})
	}
	return p.PieceImpl.MarkNotComplete()
}

// zeroChunks lists the 16 KiB chunks of got that are zeros where want is not.
func zeroChunks(got, want []byte) []int {
	const chunk = 16 << 10
	var idx []int
	for i := 0; i+chunk <= len(got) && i+chunk <= len(want); i += chunk {
		if bytes.Equal(got[i:i+chunk], make([]byte, chunk)) && !bytes.Equal(want[i:i+chunk], got[i:i+chunk]) {
			idx = append(idx, i/chunk)
		}
	}
	return idx
}
