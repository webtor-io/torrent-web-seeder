package services

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"google.golang.org/grpc/metadata"

	logrusmiddleware "github.com/bakins/logrus-middleware"
	"github.com/pkg/errors"

	pb "github.com/webtor-io/torrent-web-seeder/torrent-web-seeder"
)

type StatWeb struct {
	st *Stat
}

func NewStatWeb(st *Stat) *StatWeb {
	return &StatWeb{
		st: st,
	}
}

func (s *StatWeb) Serve(w http.ResponseWriter, r *http.Request, h string, p string) error {
	ha, ok := w.(*logrusmiddleware.Handler)
	if !ok {
		return errors.Errorf("unable to get writer")
	}

	f, ok := ha.ResponseWriter.(http.Flusher)
	if !ok {
		return errors.Errorf("streaming unsupported")
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	ctx := metadata.NewIncomingContext(r.Context(), metadata.MD{
		"info-hash": []string{h},
	})
	stream := NewStatStreamServer(ctx, ha, f)
	ticker := time.NewTicker(10 * time.Second)
	go func() {
		for range ticker.C {
			stream.Ping()
		}
	}()
	err := s.st.StatStream(&pb.StatRequest{Path: p}, stream)
	ticker.Stop()
	return err
}

type StatStreamServer struct {
	ctx     context.Context
	w       http.ResponseWriter
	f       http.Flusher
	counter int
	mux     sync.Mutex
}

func NewStatStreamServer(ctx context.Context, w http.ResponseWriter, f http.Flusher) *StatStreamServer {
	return &StatStreamServer{
		ctx: ctx,
		w:   w,
		f:   f,
	}
}

func (s *StatStreamServer) Context() context.Context {
	return s.ctx
}

func (s *StatStreamServer) RecvMsg(m interface{}) error {
	return errors.Errorf("not implemented")
}

func (s *StatStreamServer) SendMsg(m interface{}) error {
	return errors.Errorf("not implemented")
}

func (s *StatStreamServer) SendHeader(m metadata.MD) error {
	return errors.Errorf("not implemented")
}

func (s *StatStreamServer) SetHeader(m metadata.MD) error {
	return errors.Errorf("not implemented")
}

func (s *StatStreamServer) SetTrailer(m metadata.MD) {}

func (s *StatStreamServer) Ping() {
	s.mux.Lock()
	defer s.mux.Unlock()

	fmt.Fprintf(s.w, "id: %v\n", s.counter)
	fmt.Fprintf(s.w, "event: ping\n")
	fmt.Fprintf(s.w, "data: %v\n\n", time.Now().Unix())
	s.counter++
	s.f.Flush()
}

func (s *StatStreamServer) Send(m *pb.StatReply) error {
	s.mux.Lock()
	defer s.mux.Unlock()
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	fmt.Fprintf(s.w, "id: %v\n", s.counter)
	fmt.Fprintf(s.w, "event: statupdate\n")
	fmt.Fprintf(s.w, "data: %s\n\n", string(data))
	s.counter++
	s.f.Flush()
	return nil
}
