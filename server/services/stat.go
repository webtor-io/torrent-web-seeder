package services

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/pkg/errors"
	"github.com/urfave/cli"

	"google.golang.org/grpc"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"

	pb "bitbucket.org/vintikzzzz/torrent-web-seeder/torrent-web-seeder"
	"github.com/anacrolix/torrent"
	log "github.com/sirupsen/logrus"

	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpc_logrus "github.com/grpc-ecosystem/go-grpc-middleware/logging/logrus"
	grpc_recovery "github.com/grpc-ecosystem/go-grpc-middleware/recovery"
	grpc_ctxtags "github.com/grpc-ecosystem/go-grpc-middleware/tags"
)

type Stat struct {
	mux    sync.Mutex
	ts     *Torrent
	s      *grpc.Server
	host   string
	port   int
	l      net.Listener
	inited bool
	err    error
}

const (
	STAT_HOST_FLAG = "stat-host"
	STAT_PORT_FLAG = "stat-port"
)

func RegisterStatFlags(c *cli.App) {
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:  STAT_HOST_FLAG,
		Usage: "stat listening host",
		Value: "",
	})
	c.Flags = append(c.Flags, cli.IntFlag{
		Name:  STAT_PORT_FLAG,
		Usage: "stat listening port",
		Value: 50051,
	})
}

func NewStat(c *cli.Context, ts *Torrent) *Stat {
	return &Stat{ts: ts, host: c.String(STAT_HOST_FLAG), port: c.Int(STAT_PORT_FLAG), inited: false}
}

func (ss *Stat) Serve() error {
	s, err := ss.Get()
	if err != nil {
		return err
	}
	addr := fmt.Sprintf("%s:%d", ss.host, ss.port)
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return errors.Wrap(err, "Failed to listen to tcp connection")
	}
	ss.l = l
	log.Infof("Serving Stat at %v", addr)
	return s.Serve(l)
}

func (ss *Stat) Close() {
	if ss.l != nil {
		ss.l.Close()
	}
}

func (ss *Stat) get() (*grpc.Server, error) {
	log.Info("Initializing Stat")
	grpcLog := log.WithFields(log.Fields{})
	alwaysLoggingDeciderServer := func(ctx context.Context, fullMethodName string, servingObject interface{}) bool { return true }
	s := grpc.NewServer(
		grpc.StreamInterceptor(grpc_middleware.ChainStreamServer(
			grpc_ctxtags.StreamServerInterceptor(),
			grpc_logrus.StreamServerInterceptor(grpcLog),
			grpc_logrus.PayloadStreamServerInterceptor(grpcLog, alwaysLoggingDeciderServer),
			grpc_recovery.StreamServerInterceptor(),
		)),
		grpc.UnaryInterceptor(grpc_middleware.ChainUnaryServer(
			grpc_ctxtags.UnaryServerInterceptor(),
			grpc_logrus.UnaryServerInterceptor(grpcLog),
			grpc_logrus.PayloadUnaryServerInterceptor(grpcLog, alwaysLoggingDeciderServer),
			grpc_recovery.UnaryServerInterceptor(),
		)),
	)
	pb.RegisterTorrentWebSeederServer(s, &grpcServer{ts: ss.ts})
	reflection.Register(s)
	return s, nil
}

func (s *Stat) Get() (*grpc.Server, error) {
	s.mux.Lock()
	defer s.mux.Unlock()
	if s.inited {
		return s.s, s.err
	}
	s.s, s.err = s.get()
	s.inited = true
	return s.s, s.err
}

type grpcServer struct {
	ts *Torrent
}

func peers(t *torrent.Torrent) int {
	return len(t.KnownSwarm())
}

func (s *grpcServer) commonStat(t *torrent.Torrent) (*pb.StatReply, error) {
	return &pb.StatReply{
		Completed: t.BytesCompleted(),
		Total:     t.Info().TotalLength(),
		Peers:     int32(peers(t)),
		Status:    pb.StatReply_SEEDING,
	}, nil
}

func fileBytesCompleted(t *torrent.Torrent, f *torrent.File) int64 {
	var res int64
	for _, p := range f.State() {
		if p.Complete {
			res += p.Bytes
		}
	}
	return res
}

func (s *grpcServer) fileStat(t *torrent.Torrent, f *torrent.File) (*pb.StatReply, error) {
	completed := fileBytesCompleted(t, f)
	status := pb.StatReply_SEEDING
	if completed == 0 {
		status = pb.StatReply_WAITING_FOR_PEERS
	}
	pieces := []*pb.Piece{}
	for i, p := range f.State() {
		pieces = append(pieces, &pb.Piece{Position: int64(i), Complete: p.Complete})
	}
	return &pb.StatReply{
		Completed: completed,
		Total:     f.FileInfo().Length,
		Peers:     int32(peers(t)),
		Status:    status,
		Pieces:    pieces,
	}, nil
}

func findFile(t *torrent.Torrent, path string) *torrent.File {
	for _, f := range t.Files() {
		if f.Path() == path {
			return f
		}
	}
	return nil
}

func (s *grpcServer) Stat(ctx context.Context, in *pb.StatRequest) (*pb.StatReply, error) {
	if !s.ts.Ready() {
		return &pb.StatReply{
			Status: pb.StatReply_INITIALIZATION,
		}, nil
	}
	t, err := s.ts.Get()
	if err != nil {
		return nil, err
	}
	if in.GetPath() == "" {
		return s.commonStat(t)
	}
	f := findFile(t, in.GetPath())
	if f == nil {
		return nil, status.Errorf(codes.NotFound, "Unable to find file for path=%v", in.GetPath())
	}
	return s.fileStat(t, f)
}

func diff(a []*pb.Piece, b []*pb.Piece) []*pb.Piece {
	d := []*pb.Piece{}
	for _, aa := range a {
		found := false
		for _, bb := range b {
			if aa.GetPosition() == bb.GetPosition() && aa.GetComplete() == bb.GetComplete() {
				found = true
				break
			}
		}
		if !found {
			d = append(d, aa)
		}
	}
	return d
}

func (s *grpcServer) StatStream(in *pb.StatRequest, stream pb.TorrentWebSeeder_StatStreamServer) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	errCh := make(chan error)
	go func() {
		var prevRep *pb.StatReply
		for range ticker.C {
			rep, err := s.Stat(nil, in)
			if err != nil {
				log.WithError(err).Error("Failed to get stat")
				errCh <- err
			}
			if prevRep != nil &&
				rep.GetCompleted() == prevRep.GetCompleted() &&
				rep.GetPeers() == prevRep.GetPeers() {
				continue
			}
			var diffPieces []*pb.Piece
			if prevRep == nil {
				diffPieces = rep.GetPieces()
			} else {
				diffPieces = diff(rep.GetPieces(), prevRep.GetPieces())
			}
			prevRep = rep
			diffRep := &pb.StatReply{
				Completed: rep.GetCompleted(),
				Peers:     rep.GetPeers(),
				Status:    rep.GetStatus(),
				Total:     rep.GetTotal(),
				Pieces:    diffPieces,
			}
			log.WithField("rep", diffRep).WithField("path", in.GetPath()).Info("Sending stat")
			if err := stream.Send(diffRep); err != nil {
				log.WithError(err).Error("Failed to send stat")
				errCh <- err
			}
			if rep.GetTotal() == rep.GetCompleted() {
				errCh <- nil
				break
			}
		}
	}()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-sigs:
		stream.Send(&pb.StatReply{
			Status: pb.StatReply_TERMINATED,
		})
		return nil
	case <-stream.Context().Done():
		err := stream.Context().Err()
		if err != nil {
			log.WithError(err).Error("Failed to send stat")
		} else {
			log.WithField("path", in.GetPath()).Info("Sending stats completed")
		}
		return err
	case err := <-errCh:
		return err
	}
}

func (s *grpcServer) Files(ctx context.Context, in *pb.FilesRequest) (*pb.FilesReply, error) {
	t, err := s.ts.Get()
	if err != nil {
		return nil, err
	}
	var fs []*pb.File
	for _, f := range t.Files() {
		fs = append(fs, &pb.File{Path: f.Path()})
	}
	return &pb.FilesReply{Files: fs}, nil
}
