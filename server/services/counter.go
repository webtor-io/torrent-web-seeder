package services

import (
	"net/http"
	"sync/atomic"
)

type Counter struct {
	count uint64
}

func NewCounter() *Counter {
	return &Counter{}
}

type ResponseWriterCounter struct {
	w http.ResponseWriter
	c *Counter
}

func (s *ResponseWriterCounter) Write(buf []byte) (int, error) {
	n, err := s.w.Write(buf)
	s.c.Add(uint64(n))
	return n, err
}

func (s *ResponseWriterCounter) WriteHeader(code int) {
	s.w.WriteHeader(code)
}

func (s *ResponseWriterCounter) Header() http.Header {
	return s.w.Header()
}

func (s *Counter) Add(n uint64) {
	atomic.AddUint64(&s.count, n)
}

func (s *Counter) Count() uint64 {
	return s.count
}

func (s *Counter) NewResponseWriter(w http.ResponseWriter) *ResponseWriterCounter {
	return &ResponseWriterCounter{c: s, w: w}
}
