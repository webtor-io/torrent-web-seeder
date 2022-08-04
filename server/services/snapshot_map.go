package services

import (
	"net/http"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	cs "github.com/webtor-io/common-services"
	"github.com/webtor-io/lazymap"
)

type SnapshotMap struct {
	tm *TorrentMap
	s3 *cs.S3Client
	c  *cli.Context
	lazymap.LazyMap
}

func NewSnapshotMap(c *cli.Context, tm *TorrentMap, s3 *cs.S3Client) *SnapshotMap {
	return &SnapshotMap{
		tm: tm,
		c:  c,
		s3: s3,
		LazyMap: lazymap.New(&lazymap.Config{
			Expire: time.Hour,
		}),
	}
}

func (s *SnapshotMap) get(h string) (*Snapshot, error) {
	t, err := s.tm.Get(h)
	if err != nil {
		return nil, err
	}

	logger := log.WithField("info-hash", h)
	return NewSnapshot(s.c, t, s.s3, logger)
}

func (s *SnapshotMap) Get(h string) (*Snapshot, error) {
	sn, err := s.LazyMap.Get(h, func() (interface{}, error) {
		return s.get(h)
	})
	if err != nil {
		return nil, err
	}
	return sn.(*Snapshot), nil
}

func (s *SnapshotMap) WrapWriter(w http.ResponseWriter, h string) (http.ResponseWriter, error) {
	sn, err := s.Get(h)
	if err != nil {
		return nil, err
	}
	if sn == nil {
		return w, nil
	}
	s.LazyMap.Touch(h)
	return NewSnapshotResponseWriter(w, sn, s, h), nil
}

type SnapshotResponseWriter struct {
	w  http.ResponseWriter
	s  *Snapshot
	sm *SnapshotMap
	h  string
}

func (s *SnapshotResponseWriter) Write(buf []byte) (int, error) {
	s.sm.Touch(s.h)
	n, err := s.w.Write(buf)
	s.s.Add(int64(n))
	return n, err
}

func (s *SnapshotResponseWriter) WriteHeader(code int) {
	s.w.WriteHeader(code)
}

func (s *SnapshotResponseWriter) Header() http.Header {
	return s.w.Header()
}

func NewSnapshotResponseWriter(w http.ResponseWriter, sn *Snapshot, sm *SnapshotMap, h string) *SnapshotResponseWriter {
	return &SnapshotResponseWriter{s: sn, sm: sm, w: w, h: h}
}
