package services

import (
	"sync"
	"time"

	"github.com/pkg/errors"

	"code.cloudfoundry.org/bytefmt"

	"github.com/juju/ratelimit"
)

const (
	BUCKET_TTL = 30 * 60
)

type BucketPool struct {
	sm     sync.Map
	timers sync.Map
	expire time.Duration
}

func NewBucketPool() *BucketPool {
	return &BucketPool{expire: time.Duration(BUCKET_TTL) * time.Second}
}

func (s *BucketPool) Get(sessionID string, rate string) (*ratelimit.Bucket, error) {
	key := sessionID + rate
	r, err := bytefmt.ToBytes(rate)
	if err != nil {
		return nil, errors.Errorf("failed to parse rate %v", rate)
	}

	v, _ := s.sm.LoadOrStore(key, ratelimit.NewBucketWithRate(float64(r)/8, int64(r)))
	t, tLoaded := s.timers.LoadOrStore(key, time.NewTimer(s.expire))
	timer := t.(*time.Timer)
	if !tLoaded {
		go func() {
			<-timer.C
			s.sm.Delete(key)
			s.timers.Delete(key)
		}()
	} else {
		timer.Reset(s.expire)
	}

	return v.(*ratelimit.Bucket), nil
}
