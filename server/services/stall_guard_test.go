package services

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	log "github.com/sirupsen/logrus"
)

// A stream with no progress is cancelled after the timeout; one that keeps
// writing is not; a disabled timeout never cancels.
func TestWatchStall(t *testing.T) {
	logger := log.WithField("test", "stall")

	// No progress at all: cancelled once idle > timeout.
	ctx, cancel := context.WithCancel(context.Background())
	go watchStall(ctx, cancel, func() time.Time { return time.Time{} }, 60*time.Millisecond, logger)
	select {
	case <-ctx.Done():
	case <-time.After(400 * time.Millisecond):
		t.Fatal("stalled stream was not cancelled")
	}

	// Progress keeps it alive.
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	stop := make(chan struct{})
	go func() {
		tk := time.NewTicker(10 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				last.Store(time.Now().UnixNano())
			}
		}
	}()
	go watchStall(ctx2, cancel2, func() time.Time { return time.Unix(0, last.Load()) }, 60*time.Millisecond, logger)
	time.Sleep(250 * time.Millisecond)
	close(stop)
	if ctx2.Err() != nil {
		t.Fatal("a progressing stream must not be cancelled")
	}

	// Disabled.
	ctx3, cancel3 := context.WithCancel(context.Background())
	defer cancel3()
	go watchStall(ctx3, cancel3, func() time.Time { return time.Time{} }, 0, logger)
	time.Sleep(100 * time.Millisecond)
	if ctx3.Err() != nil {
		t.Fatal("timeout 0 must disable the guard")
	}
}

// TouchWriter records the time of its last write.
func TestTouchWriter_LastWrite(t *testing.T) {
	tm := &TorrentMap{entries: map[string]*torrentEntry{}, ttl: time.Second}
	w := NewTouchWriter(httptest.NewRecorder(), tm, nil, "h")
	if !w.LastWrite().IsZero() {
		t.Fatal("no write yet must read as zero time")
	}
	before := time.Now()
	if _, err := w.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if w.LastWrite().Before(before) {
		t.Fatal("LastWrite must be updated by Write")
	}
}

// A client that stays connected but stops reading (a paused player, a proxy
// whose own client stopped) blocks the response in Write. watchStall's cancel
// reaches the torrent reader or the cached file, not the Write, so the handler
// stayed until the client went away: on the torrent path its Hold kept the
// torrent loaded and its dir held. 2026-10-05: a stalled request kept its
// torrent 75 times for an hour or more, 8 times for six hours or more; at 18:30Z
// 73dfedcd on worker63 had been kept every 10 min since its stream stalled at
// 09:40 ("stream stalled, cancelling", no "completed handling request").
func TestStalledWriteEndsTheResponse(t *testing.T) {
	const (
		pieceLen = 1 << 20
		pieces   = 32 // far more than the socket buffers hold
	)
	for _, tc := range []struct {
		name   string
		onDisk int
		budget int64
	}{
		{"cache path", pieces, 0},
		// All but the last piece on disk and a budget of what is there:
		// eviction is on, so the cache path leaves the file to the torrent,
		// and nothing is evicted.
		{"torrent path", pieces - 1, pieceLen * (pieces - 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := &metainfo.Info{Name: "big.bin", PieceLength: pieceLen, Length: pieceLen * pieces, Pieces: makeDummyPieces(pieces)}
			ib, err := bencode.Marshal(info)
			if err != nil {
				t.Fatal(err)
			}
			mi := &metainfo.MetaInfo{InfoBytes: ib}
			ih := mi.HashInfoBytes()
			dataDir := t.TempDir()
			impl, err := NewMMap(dataDir, 0, FileCacheConfig{}).OpenTorrent(context.Background(), info, ih)
			if err != nil {
				t.Fatal(err)
			}
			chunk := bytes.Repeat([]byte{1}, pieceLen)
			for i := 0; i < tc.onDisk; i++ {
				sp := impl.Piece(info.Piece(i))
				if _, err := sp.WriteAt(chunk, 0); err != nil {
					t.Fatal(err)
				}
				if err := sp.MarkComplete(); err != nil {
					t.Fatal(err)
				}
			}
			if err := impl.Close(); err != nil {
				t.Fatal(err)
			}

			ws, _ := zPod(t, dataDir, mi, tc.budget, 300*time.Millisecond)
			h := ih.HexString()
			served := make(chan struct{})
			srv := httptest.NewUnstartedServer(nil)
			srv.Config = newServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(served)
				ws.ServeHTTP(w, r)
			}))
			srv.Start()
			t.Cleanup(srv.Close)

			c, err := net.Dial("tcp", srv.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			_ = c.(*net.TCPConn).SetReadBuffer(4 << 10)
			if _, err := fmt.Fprintf(c, "GET /%s/big.bin?download=true HTTP/1.1\r\nHost: seeder\r\n\r\n", h); err != nil {
				t.Fatal(err)
			}
			// Nothing is read from here on.
			select {
			case <-served:
				if ws.tm.reading(h) {
					t.Error("the torrent is still held")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("a response its client stopped reading still runs 5 s after its 300 ms stall timeout")
			}
		})
	}
}
