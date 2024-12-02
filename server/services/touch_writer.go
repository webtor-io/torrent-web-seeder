package services

import (
	"bufio"
	"github.com/pkg/errors"
	"net"
	"net/http"
)

type TouchWriter struct {
	http.ResponseWriter
	tm *TorrentMap
	h  string
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
