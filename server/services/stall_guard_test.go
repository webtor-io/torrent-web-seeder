package services

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	log "github.com/sirupsen/logrus"
	"golang.org/x/time/rate"
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

// A download is written a whole piece at a time (f76384a). The swarm's data
// for the piece it waits on is progress: a piece that takes longer than the
// stall timeout no longer ends the stream (see waitingReader). A swarm that
// sends nothing still does.
func TestStallGuardOnTheSwarm(t *testing.T) {
	const (
		pieceLen = 1 << 20 // 64 chunks
		timeout  = time.Second
	)
	data, mi := multiChunkPayload(t, pieceLen, 1)
	h := mi.HashInfoBytes().HexString()
	for _, tc := range []struct {
		name string
		// chunks per second the one peer sends; 0: no peer
		rate int
	}{
		// ~3.2 s for the piece, a chunk every 50 ms.
		{"slow swarm", 20},
		{"silent swarm", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws, _ := zPod(t, t.TempDir(), mi, 0, timeout)
			if tc.rate > 0 {
				peer := seedingClient(t, data, mi, throttle(tc.rate))
				tor, err := ws.tm.Get(context.Background(), h)
				if err != nil {
					t.Fatal(err)
				}
				tor.AddClientPeer(peer)
			}
			srv := httptest.NewServer(ws)
			t.Cleanup(srv.Close)
			start := time.Now()
			client := &http.Client{Timeout: 20 * timeout}
			resp, err := client.Get(fmt.Sprintf("%s/%s/payload.bin?download=true", srv.URL, h))
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			took := time.Since(start)
			if tc.rate > 0 && !bytes.Equal(body, data) {
				t.Fatalf("got %d of %d bytes in %v (%v): the stream was cut while its piece arrived", len(body), len(data), took.Round(time.Millisecond), err)
			}
			if tc.rate == 0 && (len(body) != 0 || took > 4*timeout) {
				t.Fatalf("got %d bytes in %v, stall timeout %v", len(body), took.Round(time.Millisecond), timeout)
			}
		})
	}
}

// Two peers that send only bad data for the piece a download waits on: its
// hash fails, the library bans neither (it bans a piece's sole toucher), and
// the piece downloads again and again. Bytes arriving a second time are not
// progress, and the stream ends at the stall timeout after the first failed
// hash; when every drop counted, it ran until the client gave up.
func TestStallGuardEndsAPoisonedSwarm(t *testing.T) {
	const (
		pieceLen = 1 << 20 // 64 chunks
		timeout  = time.Second
	)
	_, mi := multiChunkPayload(t, pieceLen, 1)
	h := mi.HashInfoBytes().HexString()
	ws, _ := zPod(t, t.TempDir(), mi, 0, timeout)
	tor, err := ws.tm.Get(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	// ~1.6 s for the piece from the two together.
	for range 2 {
		junk := make([]byte, pieceLen)
		if _, err := rand.Read(junk); err != nil {
			t.Fatal(err)
		}
		tor.AddClientPeer(seedModeClient(t, junk, mi, throttle(20)))
	}
	srv := httptest.NewServer(ws)
	t.Cleanup(srv.Close)
	start := time.Now()
	client := &http.Client{Timeout: 20 * timeout}
	resp, err := client.Get(fmt.Sprintf("%s/%s/payload.bin?download=true", srv.URL, h))
	var n int
	if err == nil {
		var body []byte
		body, err = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		n = len(body)
	}
	took := time.Since(start)
	st := tor.Stats()
	bad := st.PiecesDirtiedBad.Int64()
	t.Logf("%d bytes in %v (%v), %d failed hashes", n, took.Round(time.Millisecond), err, bad)
	if bad == 0 {
		t.Fatal("no hash failed: nothing here tests a poisoned swarm")
	}
	if n != 0 || took > 5*timeout {
		t.Fatalf("a swarm that sends only bad data held the stream %v (%d bytes), stall timeout %v", took.Round(time.Millisecond), n, timeout)
	}
}

// A client that stops reading holds the handler in Write, and meanwhile the
// reader's readahead downloads the piece after the one written. That data is
// not the stream's progress: the stream ends at the stall timeout, as it did
// when only writes counted (TestStalledWriteEndsTheResponse, where nothing
// downloads meanwhile).
func TestStallGuardEndsASilentClientOnASlowSwarm(t *testing.T) {
	const (
		pieceLen = 1 << 20 // 64 chunks
		timeout  = time.Second
	)
	data, mi := multiChunkPayload(t, pieceLen, 2)
	h := mi.HashInfoBytes().HexString()
	info, err := mi.UnmarshalInfo()
	if err != nil {
		t.Fatal(err)
	}
	// Piece 0 is on the pod, piece 1 comes from the peer in ~3.2 s.
	dataDir := t.TempDir()
	impl, err := NewMMap(dataDir, 0, FileCacheConfig{}).OpenTorrent(context.Background(), &info, mi.HashInfoBytes())
	if err != nil {
		t.Fatal(err)
	}
	sp := impl.Piece(info.Piece(0))
	if _, err := sp.WriteAt(data[:pieceLen], 0); err != nil {
		t.Fatal(err)
	}
	if err := sp.MarkComplete(); err != nil {
		t.Fatal(err)
	}
	if err := impl.Close(); err != nil {
		t.Fatal(err)
	}
	ws, _ := zPod(t, dataDir, mi, 0, timeout)
	tor, err := ws.tm.Get(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	tor.AddClientPeer(seedingClient(t, data, mi, throttle(20)))

	// The first read takes piece 0's last 32 KiB, and its write is the one
	// the client does not take: the reader waits at piece 1.
	aborted := make(chan struct{})
	req := httptest.NewRequest(http.MethodGet, "/"+h+"/payload.bin?download=true", nil)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-", pieceLen-32<<10))
	req = req.WithContext(context.WithValue(req.Context(), connKey{}, abortConn{aborted: aborted}))
	start := time.Now()
	served := make(chan struct{})
	go func() {
		defer close(served)
		ws.ServeHTTP(&stuckWriter{h: http.Header{}, writing: make(chan struct{}), unblock: aborted}, req)
	}()
	select {
	case <-served:
	case <-time.After(20 * timeout):
		t.Fatal("the response still runs")
	}
	if took := time.Since(start); took > 5*timeout/2 {
		t.Fatalf("a client that stopped reading was cut after %v, stall timeout %v", took.Round(time.Millisecond), timeout)
	}
	if left := tor.PieceBytesMissing(1); left == pieceLen {
		t.Fatal("piece 1 got no data while the handler was in Write: nothing here tests that such data is not progress")
	}
}

// throttle limits a client's upload to chunks 16 KiB chunks a second.
func throttle(chunks int) func(*torrent.ClientConfig) {
	return func(cfg *torrent.ClientConfig) {
		cfg.UploadRateLimiter = rate.NewLimiter(rate.Limit(chunks*16<<10), 16<<10)
	}
}

// abortConn stands for a request's connection: abortWrite's deadline closes
// aborted, which ends a stuckWriter's Write.
type abortConn struct {
	net.Conn
	aborted chan struct{}
}

func (c abortConn) SetWriteDeadline(time.Time) error {
	close(c.aborted)
	return nil
}

// Data is progress only for the piece a Read waits on, only as it arrives
// for the first time in that Read, and only while the Read is in flight: a
// handler held in Write by a client that stopped reading gets nothing from
// the readahead filling meanwhile.
func TestWaitingReaderLastData(t *testing.T) {
	const pieces = 3
	var mu sync.Mutex
	missing := map[int]int64{0: 100, 1: 100, 2: 100}
	set := func(piece int, left int64) {
		mu.Lock()
		missing[piece] = left
		mu.Unlock()
	}
	reads := make(chan int)
	r := &waitingReader{
		ReadSeekCloser: fakeTorrentReader(reads),
		missing: func(piece int) int64 {
			if piece >= pieces {
				t.Errorf("asked for piece %d of %d", piece, pieces)
			}
			mu.Lock()
			defer mu.Unlock()
			return missing[piece]
		},
		length:   pieces * 100,
		pieceLen: 100,
	}
	read := func() {
		go func() { _, _ = r.Read(make([]byte, 1)) }()
		for !r.reading.Load() {
			time.Sleep(time.Millisecond)
		}
	}
	// returns ends the Read in flight with n bytes.
	returns := func(n int) {
		pos := r.pos.Load()
		reads <- n
		for r.pos.Load() == pos {
			time.Sleep(time.Millisecond)
		}
	}

	read() // waits on piece 0
	if !r.lastData().IsZero() {
		t.Fatal("no data has arrived yet")
	}
	set(1, 50)
	set(0, 120) // lost bytes: a failed hash, an eviction
	if !r.lastData().IsZero() {
		t.Fatal("another piece's data, or a piece losing bytes, is not data for this read")
	}
	set(0, 60)
	got := r.lastData()
	if got.IsZero() {
		t.Fatal("data for the piece the read waits on is progress")
	}
	set(0, 0) // every byte in, the hash pending
	got = r.lastData()
	set(0, 100) // the hash failed
	r.lastData()
	set(0, 30)
	if r.lastData() != got {
		t.Fatal("bytes the read already had once, arriving again after a failed hash, are progress")
	}

	set(0, 0)
	returns(40) // part of piece 0, which another pod then punches
	set(0, 100)
	read() // waits on piece 0 again
	r.lastData()
	set(0, 90)
	next := r.lastData()
	if next == got {
		t.Fatal("data for a piece a new read waits on is progress, though an earlier read saw it whole")
	}
	got = next

	set(0, 0)
	returns(60)
	read() // waits on piece 1, which has fewer bytes left than piece 0 had
	if r.lastData() != got {
		t.Fatal("a piece already part downloaded is not data arriving")
	}
	set(1, 40)
	next = r.lastData()
	if next == got {
		t.Fatal("data for the piece the next read waits on is progress")
	}
	got = next

	returns(100) // the handler is in Write, the readahead fills piece 2
	r.lastData()
	set(2, 30) // fewer than piece 1 ever missed
	if r.lastData() != got {
		t.Fatal("data that arrives while no read waits is progress")
	}

	read()
	returns(100) // at the file's end
	read()
	r.lastData()
	reads <- 0
}

// fakeTorrentReader's Read returns, per call, the count sent on it.
type fakeTorrentReader chan int

func (f fakeTorrentReader) Read([]byte) (int, error)     { return <-f, nil }
func (fakeTorrentReader) Seek(int64, int) (int64, error) { return 0, nil }
func (fakeTorrentReader) Close() error                   { return nil }
