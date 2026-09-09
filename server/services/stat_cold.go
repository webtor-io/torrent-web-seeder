package services

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/go-llsqlite/adapter"
	"github.com/go-llsqlite/adapter/sqlitex"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/webtor-io/torrent-web-seeder/proto"
)

// coldStat answers a stats request for a torrent this client does not hold:
// metainfo from the file or torrent store, piece completion from the
// on-disk db the storage keeps per torrent, nothing from the swarm. Peers
// are zero and Live is false so the client can tell "nobody is watching"
// from "no seeders".
func (s *Stat) coldStat(h, path string) (*pb.StatReply, error) {
	mi, err := s.tm.MetaInfo(h)
	if err != nil {
		return nil, err
	}
	if mi == nil {
		return nil, status.Errorf(codes.NotFound, "torrent not found infoHash=%v", h)
	}
	info, err := mi.UnmarshalInfo()
	if err != nil {
		return nil, err
	}
	return coldReply(&info, s.readCompletion(h, info.NumPieces()), path)
}

// readCompletion loads the completed-piece flags for h from
// <dir>/.torrent.db (piece_completion.go writes it). No dir or no db means
// nothing was ever stored on this node: all false.
func (s *Stat) readCompletion(h string, numPieces int) []bool {
	complete := make([]bool, numPieces)
	if s.dataDir == "" {
		return complete
	}
	dir, err := GetDir(s.dataDir, h)
	if err != nil {
		return complete
	}
	p := filepath.Join(dir, ".torrent.db")
	if _, err := os.Stat(p); err != nil {
		return complete
	}
	db, err := sqlite.OpenConn(p, sqlite.OpenReadOnly)
	if err != nil {
		log.WithError(err).WithField("path", p).Debug("cold stat: cannot open piece completion db")
		return complete
	}
	defer db.Close()
	err = sqlitex.Exec(db, `select "index", complete from piece_completion`, func(stmt *sqlite.Stmt) error {
		i := stmt.ColumnInt(0)
		if i >= 0 && i < numPieces {
			complete[i] = stmt.ColumnInt(1) != 0
		}
		return nil
	})
	if err != nil {
		log.WithError(err).WithField("path", p).Debug("cold stat: cannot read piece completion")
	}
	return complete
}

// coldSpan is a byte range of the torrent to report on: the whole torrent,
// one file, or a directory's files end to end.
type coldSpan struct {
	offset, length int64
}

// coldReply builds the same shape as torrentStat/fileStat/dirStat from
// metainfo geometry and completed-piece flags alone. Positions are relative
// to the span, as the live variants report them.
func coldReply(info *metainfo.Info, complete []bool, path string) (*pb.StatReply, error) {
	pieceLen := info.PieceLength
	if pieceLen <= 0 {
		return nil, status.Errorf(codes.Internal, "piece length is zero")
	}
	totalLen := info.TotalLength()
	span, ok := coldSpanFor(info, path)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "unable to find file for path=%v", path)
	}
	begin, end := dirPieceRange(pieceLen, span.offset, span.length)
	if n := info.NumPieces(); end > n {
		end = n
	}
	var completed int64
	pieces := make([]*pb.Piece, 0, end-begin)
	for i := begin; i < end; i++ {
		done := i < len(complete) && complete[i]
		pieces = append(pieces, &pb.Piece{Position: int64(i - begin), Complete: done, Priority: pb.Piece_NONE})
		if !done {
			continue
		}
		pStart := int64(i) * pieceLen
		pEnd := pStart + pieceLen
		if pEnd > totalLen {
			pEnd = totalLen
		}
		// Overlap of the piece with the span.
		lo, hi := pStart, pEnd
		if lo < span.offset {
			lo = span.offset
		}
		if hi > span.offset+span.length {
			hi = span.offset + span.length
		}
		if hi > lo {
			completed += hi - lo
		}
	}
	rStatus := pb.StatReply_SEEDING
	if completed == 0 {
		rStatus = pb.StatReply_WAITING_FOR_PEERS
	}
	return &pb.StatReply{
		Completed: completed,
		Total:     span.length,
		Status:    rStatus,
		Pieces:    pieces,
		Live:      false,
	}, nil
}

// coldSpanFor resolves path the way findFile/dirFiles do on a live torrent:
// "" is the whole torrent, an exact file path is that file, a directory is
// its files' span. File paths carry the torrent name as their first
// segment, single-file torrents are addressed by the name itself.
func coldSpanFor(info *metainfo.Info, path string) (coldSpan, bool) {
	if path == "" {
		return coldSpan{0, info.TotalLength()}, true
	}
	var offset int64
	first, last := int64(-1), int64(0)
	for _, fi := range info.UpvertedFiles() {
		var fp string
		if len(fi.Path) == 0 {
			fp = info.Name
		} else {
			fp = info.Name + "/" + strings.Join(fi.Path, "/")
		}
		if fp == path {
			return coldSpan{offset, fi.Length}, true
		}
		if isUnderDir(fp, path) {
			if first < 0 {
				first = offset
			}
			last = offset + fi.Length
		}
		offset += fi.Length
	}
	if first >= 0 {
		return coldSpan{first, last - first}, true
	}
	return coldSpan{}, false
}
