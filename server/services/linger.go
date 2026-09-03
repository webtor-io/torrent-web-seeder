package services

import (
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anacrolix/torrent"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

// Reader linger keeps the pieces a client was reading prioritised for a while
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
// How: not by holding the reader open — an idle reader wants nothing
// (reader.piecesUncached: ra = 0 unless a Read is in flight), which is why
// the first version of this file did nothing observable. Instead, on Close
// the pieces of the reader's window [pos, pos+readahead) that are not yet
// complete get an explicit Normal priority (Piece.SetPriority, sticky), and
// after ReaderLinger the ones still at Normal and still incomplete are
// released to None. Pieces already at a higher explicit priority (warmup
// sets High) are left alone both ways.
//
// The cost is bounded: at most maxLingering windows per pod, each at most
// one readahead of extra download for at most the linger duration.

const (
	ReaderLingerFlag = "reader-linger"
	// maxLingering bounds the windows held past their request; beyond it a
	// Close is a plain close, as before this feature.
	maxLingering = 512
)

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
	active    atomic.Int32
	rejected  atomic.Int64
	afterFn   func(time.Duration, func()) *time.Timer // time.AfterFunc, swappable in tests
}

func NewLinger(c *cli.Context, readahead int64) *Linger {
	return newLinger(c.Duration(ReaderLingerFlag), readahead)
}

func newLinger(d time.Duration, readahead int64) *Linger {
	return &Linger{d: d, readahead: readahead, afterFn: time.AfterFunc}
}

// Active is the number of windows currently held past their request.
func (l *Linger) Active() int32 {
	return l.active.Load()
}

// prioritizer is the slice of a torrent the linger touches; torrentPieces
// is the real one, tests use a recorder.
type prioritizer interface {
	// raise sets Normal on incomplete pieces in [begin, end) that sit below it.
	raise(begin, end int)
	// release sets None on pieces in [begin, end) still at Normal and incomplete.
	release(begin, end int)
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
		err = r.ReadSeekCloser.Close()
		begin, end := r.pieces()
		if begin >= end {
			return
		}
		l := r.l
		for {
			n := l.active.Load()
			if n >= maxLingering {
				l.rejected.Add(1)
				return
			}
			if l.active.CompareAndSwap(n, n+1) {
				break
			}
		}
		r.p.raise(begin, end)
		l.afterFn(l.d, func() {
			defer l.active.Add(-1)
			r.p.release(begin, end)
		})
	})
	return err
}

type torrentPieces struct {
	t *torrent.Torrent
}

func (tp torrentPieces) raise(begin, end int) {
	n := tp.t.NumPieces()
	for i := begin; i < end && i < n; i++ {
		st := tp.t.PieceState(i)
		if st.Complete || st.Priority >= torrent.PiecePriorityNormal {
			continue
		}
		tp.t.Piece(i).SetPriority(torrent.PiecePriorityNormal)
	}
}

func (tp torrentPieces) release(begin, end int) {
	n := tp.t.NumPieces()
	for i := begin; i < end && i < n; i++ {
		st := tp.t.PieceState(i)
		if st.Complete || st.Priority != torrent.PiecePriorityNormal {
			// Complete: nothing to release. Higher than Normal: someone else
			// (warmup) owns it. Lower: a reader must have wanted it and it
			// was released already, or never raised.
			continue
		}
		tp.t.Piece(i).SetPriority(torrent.PiecePriorityNone)
	}
	log.WithField("pieces", end-begin).Debug("linger: window released")
}
