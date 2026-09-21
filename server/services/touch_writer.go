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
	tm *TorrentMap
	h  string
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

func NewTouchWriter(w http.ResponseWriter, tm *TorrentMap, h string) *TouchWriter {
	return &TouchWriter{
		ResponseWriter: w,
		tm:             tm,
		h:              h,
	}
}

func (w *TouchWriter) WriteHeader(statusCode int) {
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *TouchWriter) Write(p []byte) (int, error) {
	w.tm.Touch(w.h)
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
