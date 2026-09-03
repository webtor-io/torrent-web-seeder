package services

import (
	"io"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

// Reader linger keeps a torrent reader open for a while after the HTTP
// request that used it has ended.
//
// Why: anacrolix derives a piece's priority from the readers positioned on
// it (plus explicit file/piece priorities the seeder does not set). When the
// client drops the connection, the reader is closed, deleteReader recomputes
// priorities, the piece falls to None and every outstanding peer request for
// it is cancelled. A client whose idle timeout is shorter than the swarm's
// time-to-first-byte — download managers give up after 30 s of silence —
// then retries the same offset against a piece that has been abandoned, and
// the download never advances: one course torrent produced 1495 attempts and
// one completion in three hours (2026-09-03). Holding the reader for
// ReaderLinger after Close keeps its Now/Readahead pieces prioritised, so the
// piece finishes and the retry is served.
//
// The cost is bounded: at most maxLingering readers per pod, each keeping up
// to the reader's readahead window in flight for at most the linger
// duration. A torrent dropped by TorrentMap invalidates its readers anyway;
// the deferred Close then just releases the slot.

const (
	ReaderLingerFlag = "reader-linger"
	// maxLingering bounds the readers held past their request; beyond it a
	// Close is immediate, as before this feature.
	maxLingering = 512
)

func RegisterLingerFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.DurationFlag{
			Name:   ReaderLingerFlag,
			Usage:  "keep a torrent reader (and the piece priorities it implies) open this long after its HTTP request ends, so a client that timed out and retries finds the piece progressed; 0 disables",
			Value:  90 * time.Second,
			EnvVar: "READER_LINGER",
		},
	)
}

// Linger wraps torrent readers so their Close is deferred.
type Linger struct {
	d        time.Duration
	active   atomic.Int32
	afterFn  func(time.Duration, func()) *time.Timer // time.AfterFunc, swappable in tests
	rejected atomic.Int64
}

func NewLinger(c *cli.Context) *Linger {
	return newLinger(c.Duration(ReaderLingerFlag))
}

func newLinger(d time.Duration) *Linger {
	return &Linger{d: d, afterFn: time.AfterFunc}
}

// Wrap returns r unchanged when linger is off; otherwise a reader whose
// Close schedules the real Close after the linger duration. Reads and Seeks
// pass through untouched.
func (l *Linger) Wrap(r io.ReadSeekCloser) io.ReadSeekCloser {
	if l == nil || l.d <= 0 {
		return r
	}
	return &lingerReader{ReadSeekCloser: r, l: l}
}

// Active is the number of readers currently held past their request.
func (l *Linger) Active() int32 {
	return l.active.Load()
}

type lingerReader struct {
	io.ReadSeekCloser
	l    *Linger
	once sync.Once
}

func (r *lingerReader) Close() error {
	var err error
	r.once.Do(func() {
		l := r.l
		for {
			n := l.active.Load()
			if n >= maxLingering {
				// Pod is holding as much as it should; behave as before.
				l.rejected.Add(1)
				err = r.ReadSeekCloser.Close()
				return
			}
			if l.active.CompareAndSwap(n, n+1) {
				break
			}
		}
		l.afterFn(l.d, func() {
			defer l.active.Add(-1)
			if cerr := r.ReadSeekCloser.Close(); cerr != nil {
				log.WithError(cerr).Debug("linger: deferred reader close")
			}
		})
	})
	return err
}
