package services

import (
	"bufio"
	"github.com/pkg/errors"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

type TouchWriter struct {
	http.ResponseWriter
	tm  *TorrentMap
	tom *TouchMap
	h   string
	// lastWrite is the unix-nano time of the last Write; the stall guard
	// reads it to tell a stream that is still moving from one that is not.
	lastWrite atomic.Int64
}

// LastWrite returns when the response last made progress (zero: never).
func (w *TouchWriter) LastWrite() time.Time {
	ns := w.lastWrite.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

func NewTouchWriter(w http.ResponseWriter, tm *TorrentMap, tom *TouchMap, h string) *TouchWriter {
	return &TouchWriter{
		ResponseWriter: w,
		tm:             tm,
		tom:            tom,
		h:              h,
	}
}

func (w *TouchWriter) WriteHeader(statusCode int) {
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *TouchWriter) Write(p []byte) (int, error) {
	if w.tm != nil { // nil: a cache-path stream, which keeps no torrent loaded
		w.tm.Touch(w.h)
	}
	// <hash>.touch is the cleaner's clock of the dir's last use. A response
	// uses the dir for as long as it writes, and a free-tier download under
	// thp's 5 Mbit/s runs for up to a day: touched only at the request's
	// start, the dir was the cleaner's first pick the moment it was let go.
	// TouchMap writes the file at most once per 30 s; an error was logged at
	// the request's start.
	if w.tom != nil {
		_, _ = w.tom.Touch(w.h)
	}
	w.lastWrite.Store(time.Now().UnixNano())
	return w.ResponseWriter.Write(p)
}

func (w *TouchWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("type assertion failed http.ResponseWriter not a http.Hijacker")
	}
	return h.Hijack()
}

func (w *TouchWriter) Flush() {
	f, ok := w.ResponseWriter.(http.Flusher)
	if !ok {
		return
	}

	f.Flush()
}

// Check interface implementations.
var (
	_ http.ResponseWriter = &TouchWriter{}
	_ http.Hijacker       = &TouchWriter{}
	_ http.Flusher        = &TouchWriter{}
)
