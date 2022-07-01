package services

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/anacrolix/torrent"
	log "github.com/sirupsen/logrus"
	pb "github.com/webtor-io/torrent-web-seeder/torrent-web-seeder"
)

type Stat struct {
	ts *Torrent
}

func NewStat(ts *Torrent) *Stat {
	return &Stat{
		ts: ts,
	}
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

func (s *Stat) torrentStat(t *torrent.Torrent) (*pb.StatReply, error) {
	completed := t.BytesCompleted()
	status := pb.StatReply_SEEDING
	if completed == 0 {
		status = pb.StatReply_WAITING_FOR_PEERS
	}
	pieces := []*pb.Piece{}
	for i := 0; i < t.NumPieces(); i++ {
		p := t.Piece(i)
		ps := p.State()
		pr := pb.Piece_NONE
		if ps.Priority == torrent.PiecePriorityNormal {
			pr = pb.Piece_NORMAL
		} else if ps.Priority > torrent.PiecePriorityNormal {
			pr = pb.Piece_HIGH
		}
		pieces = append(pieces, &pb.Piece{Position: int64(i), Complete: ps.Complete, Priority: pr})

	}
	peers := t.Stats().ActivePeers
	seeders := t.Stats().ConnectedSeeders
	leechers := peers - seeders
	return &pb.StatReply{
		Completed: completed,
		Total:     t.Info().TotalLength(),
		Peers:     int32(peers),
		Status:    status,
		Seeders:   int32(seeders),
		Leechers:  int32(leechers),
		Pieces:    pieces,
	}, nil
}

func (s *Stat) fileStat(t *torrent.Torrent, f *torrent.File) (*pb.StatReply, error) {
	completed := fileBytesCompleted(t, f)
	status := pb.StatReply_SEEDING
	if completed == 0 {
		status = pb.StatReply_WAITING_FOR_PEERS
	}
	pieces := []*pb.Piece{}
	for i, p := range f.State() {
		pr := pb.Piece_NONE
		if p.Priority == torrent.PiecePriorityNormal {
			pr = pb.Piece_NORMAL
		} else if p.Priority > torrent.PiecePriorityNormal {
			pr = pb.Piece_HIGH
		}
		pieces = append(pieces, &pb.Piece{Position: int64(i), Complete: p.Complete, Priority: pr})
	}
	peers := t.Stats().ActivePeers
	seeders := t.Stats().ConnectedSeeders
	leechers := peers - seeders
	return &pb.StatReply{
		Completed: completed,
		Total:     f.FileInfo().Length,
		Peers:     int32(peers),
		Status:    status,
		Seeders:   int32(seeders),
		Leechers:  int32(leechers),
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

func (s *Stat) Stat(ctx context.Context, in *pb.StatRequest) (*pb.StatReply, error) {
	if !s.ts.Ready() {
		return &pb.StatReply{
			Completed: 0,
			Total:     0,
			Peers:     0,
			Status:    pb.StatReply_INITIALIZATION,
			Pieces:    []*pb.Piece{},
		}, nil
	}
	t, err := s.ts.Get()
	if err != nil {
		return nil, err
	}
	if in.GetPath() == "" {
		return s.torrentStat(t)
	}
	f := findFile(t, in.GetPath())
	if f == nil {
		return nil, status.Errorf(codes.NotFound, "unable to find file for path=%v", in.GetPath())
	}
	return s.fileStat(t, f)
}

func diff(a []*pb.Piece, b []*pb.Piece) []*pb.Piece {
	d := []*pb.Piece{}
	for _, aa := range a {
		found := false
		for _, bb := range b {
			if aa.GetPosition() == bb.GetPosition() && aa.GetComplete() == bb.GetComplete() && aa.GetPriority() == bb.GetPriority() {
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

func (s *Stat) StatStream(in *pb.StatRequest, stream pb.TorrentWebSeeder_StatStreamServer) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	errCh := make(chan error)
	go func() {
		var prevRep *pb.StatReply
		for ; true; <-ticker.C {
			rep, err := s.Stat(stream.Context(), in)
			if err != nil {
				log.WithError(err).Error("failed to get stat")
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
			if err := stream.Send(diffRep); err != nil {
				log.WithError(err).Error("failed to send stat")
				errCh <- err
			}
			if rep.GetTotal() == rep.GetCompleted() && rep.GetStatus() != pb.StatReply_INITIALIZATION && rep.GetStatus() != pb.StatReply_RESTORING {
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
			log.WithError(err).Error("failed to send stat")
		} else {
			log.WithField("path", in.GetPath()).Info("sending stats completed")
		}
		return err
	case err := <-errCh:
		if err != nil {
			return status.Errorf(codes.Internal, "got error=%v", err)
		}
		return nil
	}
}

func (s *Stat) Files(ctx context.Context, in *pb.FilesRequest) (*pb.FilesReply, error) {
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
