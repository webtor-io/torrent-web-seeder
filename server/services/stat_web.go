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
	log "github.com/sirupsen/logrus"

	pb "github.com/webtor-io/torrent-web-seeder/proto"
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
	// Tell nginx-ingress to disable response buffering for this SSE stream
	// only. Without this the ingress holds statupdate events in its
	// 64×128KB proxy buffers (the small JSON payloads never fill the
	// buffer) and clients see updates only after a 30s+ flush, which the
	// web-ui warmup watchdog misreads as "no peers".
	w.Header().Set("X-Accel-Buffering", "no")

	ctx := metadata.NewIncomingContext(r.Context(), metadata.MD{
		"info-hash": []string{h},
	})
	// Local cancelable context so we can stop the Ping goroutine the same
	// way StatStream's producer is stopped (sync.WaitGroup wait before the
	// handler returns). Without this, the Ping goroutine could fire one
	// final Flush after Serve had returned and net/http freed the
	// response's bufio.Writer — SIGSEGV in chunkWriter, taking the pod
	// with it. The recover() in StatStreamServer.Ping is kept as
	// belt-and-suspenders for any future call path that bypasses this
	// sync.
	pingCtx, cancel := context.WithCancel(ctx)
	stream := NewStatStreamServer(ctx, ha, f)
	ticker := time.NewTicker(10 * time.Second)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ticker.C:
				stream.Ping()
			case <-pingCtx.Done():
				return
			}
		}
	}()
	err := s.st.StatStream(&pb.StatRequest{Path: p}, stream)
	ticker.Stop()
	cancel()
	wg.Wait()
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

	if s.ctx.Err() != nil {
		return
	}
	// Belt-and-suspenders: StatWeb.Serve now wg.Wait()s on the Ping
	// goroutine via a cancelable ping context, so Flush should never
	// run after net/http has finalized the response. Keep the recover
	// against any future call path that bypasses that sync, since this
	// panic is unrecoverable at process level and would take 30+ live
	// streams down with the pod. The Warn log lets us track whether
	// the wg-sync above actually closes the race in practice — if 0
	// events for a week we can drop the recover and trust the sync.
	defer func() {
		if r := recover(); r != nil {
			log.WithField("at", "stat_web.Ping").Warnf("recovered panic: %v", r)
		}
	}()
	fmt.Fprintf(s.w, "id: %v\n", s.counter)
	fmt.Fprintf(s.w, "event: ping\n")
	fmt.Fprintf(s.w, "data: %v\n\n", time.Now().Unix())
	s.counter++
	s.f.Flush()
}

func (s *StatStreamServer) Send(m *pb.StatReply) (retErr error) {
	s.mux.Lock()
	defer s.mux.Unlock()
	if s.ctx.Err() != nil {
		return s.ctx.Err()
	}
	// Belt-and-suspenders: the StatStream handler now wg.Wait()s on its
	// producer goroutine before returning, so this Send should never run
	// after net/http has finalized the response (and zeroed the underlying
	// bufio.Writer that chunkWriter.flush would otherwise SIGSEGV on).
	// Keep the recover anyway — a future caller adding a Send path that
	// bypasses the wg sync, or any other unforeseen race, shouldn't crash
	// the whole pod and take 30+ live streams down with it.
	defer func() {
		if r := recover(); r != nil {
			log.WithField("at", "stat_web.Send").Warnf("recovered panic: %v", r)
			retErr = errors.Errorf("StatStreamServer.Send recovered from panic: %v", r)
		}
	}()
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
