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

func NewThrottledReader(r io.ReadSeeker, rate uint64) io.ReadSeeker {
	bucket := ratelimit.NewBucketWithRate(float64(rate), int64(rate))
	return ThrottledReader{r, ratelimit.Reader(r, bucket)}
}
