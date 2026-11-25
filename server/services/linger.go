package services

import (
	"io"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/urfave/cli"
)

// Reader linger keeps the pieces a client was reading wanted for a while
// after its HTTP request has ended.
//
// Why: anacrolix derives a piece's priority from the readers positioned on
// it plus explicit file/piece priorities. When the client drops the
// connection, the reader is closed, priorities are recomputed, the piece
// falls to None and every outstanding peer request for it is cancelled. A
// client whose idle timeout is shorter than the swarm's time-to-first-byte
// — download managers give up after 30 s of silence — then retries the same
// offset against a piece that has been abandoned, and the download never
// advances: one course torrent produced 1495 attempts and one completion in
// three hours (2026-09-03).
//
// What an open reader wants: [pos, pos+readahead) at Now/Readahead from its
// first Read on, whether or not a Read is in flight. reader.readContext sets
// `reading`, and only a Seek that moves the reader clears it; Close drops the
// window at once. (An earlier version of this comment said an idle reader
// wants nothing. That is false; TestLinger_LibraryOpenReaderKeepsWindow pins
// the real behaviour.) The window is held with an explicit Normal priority
// rather than by keeping the reader open, so a window left behind by a seek
// sits below the live reader's Now/Readahead instead of competing with it.
//
// How: on Close the incomplete pieces of the reader's window get a hold, and
// only then is the reader closed, so the window never passes through None.
// After ReaderLinger the hold ends.
//
// A piece has one sticky explicit priority (Piece.SetPriority), and
// PieceState shows only the effective one: the highest of explicit, file
// and reader priorities, or None while the piece is complete or hashing.
// Who set what cannot be read back, so whoever sets an explicit priority
// counts here: one hold per lingering window, and a hold that never ends for
// warmup's High (warmup.go never lowers it). A piece's first hold raises it
// to Normal (Torrent.DownloadPieces never lowers High). When its last hold
// ends it goes back to None, whether it is complete, hashing or under a
// reader. The reader keeps its own priorities, and its own Close holds its
// own window.
//
// The cost is bounded: at most maxLingering windows per pod, each at most
// one readahead of extra download for at most the linger duration.

const (
	ReaderLingerFlag = "reader-linger"
	// maxLingering bounds the windows held past their request; beyond it a
	// Close is a plain close, as before this feature.
	maxLingering = 512
)

var (
	promLingerWindows = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "torrent_web_seeder_linger_windows",
		Help: "Reader windows held wanted past their request (--reader-linger)",
	})
	promLingerRejected = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "torrent_web_seeder_linger_rejected_total",
		Help: "Closes with incomplete pieces to hold that found the linger cap full and dropped them",
	})
)

func init() {
	prometheus.MustRegister(promLingerWindows, promLingerRejected)
}

func RegisterLingerFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.DurationFlag{
			Name:   ReaderLingerFlag,
			Usage:  "keep the pieces a client was reading prioritised this long after its HTTP request ends, so a client that timed out and retries finds the piece progressed; 0 disables",
			Value:  90 * time.Second,
			EnvVar: "READER_LINGER",
		},
	)
}

// Linger wraps torrent readers so their window outlives the request.
type Linger struct {
	d         time.Duration
	readahead int64
	afterFn   func(time.Duration, func()) *time.Timer // time.AfterFunc, swappable in tests

	mu       sync.Mutex
	active   int   // windows held past their request
	rejected int64 // windows dropped at the cap
	// holds counts, per torrent and piece, who keeps the explicit priority
	// up. A torrent's entry goes when the torrent closes.
	holds map[prioritizer]map[int]int
}

func NewLinger(c *cli.Context, readahead int64) *Linger {
	return newLinger(c.Duration(ReaderLingerFlag), readahead)
}

func newLinger(d time.Duration, readahead int64) *Linger {
	return &Linger{d: d, readahead: readahead, afterFn: time.AfterFunc}
}

// Active is the number of windows currently held past their request.
func (l *Linger) Active() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.active
}

// prioritizer is the slice of a torrent the linger touches; torrentPieces
// is the real one.
type prioritizer interface {
	complete(i int) bool
	want(i int)   // explicit priority at least Normal
	unwant(i int) // explicit priority None
	closed() <-chan struct{}
}

// window is what the wrapped reader needs to map its position to pieces.
type window struct {
	fileOffset int64 // file start within the torrent
	fileLength int64
	pieceLen   int64
}

// Wrap returns r unchanged when linger is off; otherwise a reader that
// tracks its position and, on Close, hands its readahead window to the
// torrent for the linger duration. Reads and Seeks pass through.
func (l *Linger) Wrap(r io.ReadSeekCloser, t *torrent.Torrent, f *torrent.File) io.ReadSeekCloser {
	if l == nil || l.d <= 0 || t == nil || f == nil || t.Info() == nil {
		return r
	}
	return l.wrap(r, torrentPieces{t}, window{fileOffset: f.Offset(), fileLength: f.Length(), pieceLen: t.Info().PieceLength})
}

func (l *Linger) wrap(r io.ReadSeekCloser, p prioritizer, w window) io.ReadSeekCloser {
	if l == nil || l.d <= 0 || w.pieceLen <= 0 || w.fileLength <= 0 {
		return r
	}
	return &lingerReader{ReadSeekCloser: r, l: l, p: p, w: w}
}

// pin is warmup's High: it is never lowered, so its pieces hold for the
// torrent's life and no window's release drops them. Call before setting
// High: a release that ran first then cannot undo it.
func (l *Linger) pin(p prioritizer, pieces []int) {
	if l == nil || l.d <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	h := l.torrentHolds(p)
	for _, i := range pieces {
		h[i]++
	}
}

// hold holds the incomplete pieces of [begin, end) and returns them. It
// returns nil, and takes no slot, when they are all complete or the cap is
// full.
func (l *Linger) hold(p prioritizer, begin, end int) []int {
	var held []int
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := begin; i < end; i++ {
		if !p.complete(i) {
			held = append(held, i)
		}
	}
	if len(held) == 0 {
		return nil
	}
	if l.active >= maxLingering {
		l.rejected++
		promLingerRejected.Inc()
		return nil
	}
	l.active++
	promLingerWindows.Inc()
	h := l.torrentHolds(p)
	for _, i := range held {
		if h[i]++; h[i] == 1 {
			p.want(i)
		}
	}
	return held
}

// release ends a hold taken by hold. A piece whose last hold it was goes
// back to None.
func (l *Linger) release(p prioritizer, held []int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.active--
	promLingerWindows.Dec()
	h := l.holds[p]
	if h == nil {
		return // the torrent closed
	}
	for _, i := range held {
		if h[i]--; h[i] <= 0 {
			delete(h, i)
			p.unwant(i)
		}
	}
}

// torrentHolds is p's entry, created with a watcher that drops it when the
// torrent closes. Requires l.mu.
func (l *Linger) torrentHolds(p prioritizer) map[int]int {
	if h := l.holds[p]; h != nil {
		return h
	}
	if l.holds == nil {
		l.holds = map[prioritizer]map[int]int{}
	}
	h := map[int]int{}
	l.holds[p] = h
	if c := p.closed(); c != nil {
		go func() {
			<-c
			l.mu.Lock()
			delete(l.holds, p)
			l.mu.Unlock()
		}()
	}
	return h
}

type lingerReader struct {
	io.ReadSeekCloser
	l    *Linger
	p    prioritizer
	w    window
	mu   sync.Mutex
	pos  int64
	once sync.Once
}

func (r *lingerReader) Read(b []byte) (int, error) {
	n, err := r.ReadSeekCloser.Read(b)
	r.mu.Lock()
	r.pos += int64(n)
	r.mu.Unlock()
	return n, err
}

func (r *lingerReader) Seek(off int64, whence int) (int64, error) {
	n, err := r.ReadSeekCloser.Seek(off, whence)
	if err == nil {
		r.mu.Lock()
		r.pos = n
		r.mu.Unlock()
	}
	return n, err
}

// pieces is the piece range covering [pos, pos+readahead) of the file,
// clipped to the file; empty (begin == end) when nothing is left to want.
func (r *lingerReader) pieces() (begin, end int) {
	r.mu.Lock()
	pos := r.pos
	r.mu.Unlock()
	if pos < 0 {
		pos = 0
	}
	if pos >= r.w.fileLength {
		return 0, 0
	}
	from := r.w.fileOffset + pos
	to := from + r.l.readahead
	if fileEnd := r.w.fileOffset + r.w.fileLength; to > fileEnd || r.l.readahead <= 0 {
		to = fileEnd
	}
	return int(from / r.w.pieceLen), int((to-1)/r.w.pieceLen) + 1
}

func (r *lingerReader) Close() error {
	var err error
	r.once.Do(func() {
		// Hold, then close: closed first, the window would drop to None in
		// between and its peer requests be cancelled.
		if begin, end := r.pieces(); begin < end {
			if held := r.l.hold(r.p, begin, end); held != nil {
				r.l.afterFn(r.l.d, func() { r.l.release(r.p, held) })
			}
		}
		err = r.ReadSeekCloser.Close()
	})
	return err
}

type torrentPieces struct {
	t *torrent.Torrent
}

func (tp torrentPieces) complete(i int) bool     { return tp.t.PieceState(i).Complete }
func (tp torrentPieces) want(i int)              { tp.t.DownloadPieces(i, i+1) }
func (tp torrentPieces) unwant(i int)            { tp.t.CancelPieces(i, i+1) }
func (tp torrentPieces) closed() <-chan struct{} { return tp.t.Closed() }
