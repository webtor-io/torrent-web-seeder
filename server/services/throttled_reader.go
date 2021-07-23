package services

import (
	"io"

	"github.com/juju/ratelimit"
)

type ThrottledReader struct {
	io.ReadSeeker
	r io.Reader
}

func (r ThrottledReader) Read(p []byte) (int, error) {
	return r.r.Read(p)
}

func NewThrottledReader(r io.ReadSeeker, b *ratelimit.Bucket) io.ReadSeeker {
	return ThrottledReader{r, ratelimit.Reader(r, b)}
}
