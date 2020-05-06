package services

import (
	"io"

	"github.com/anacrolix/torrent"
)

type PieceReader struct {
	p *torrent.Piece
	r torrent.Reader
}

func NewPieceReader(r torrent.Reader, p *torrent.Piece) *PieceReader {
	return &PieceReader{p: p, r: r}
}

func (s *PieceReader) Read(p []byte) (n int, err error) {
	return s.r.Read(p)
}

func (s *PieceReader) Close() error {
	return s.r.Close()
}

func (s *PieceReader) Seek(offset int64, whence int) (int64, error) {
	pieceOffset := int64(s.p.Info().Offset())
	pieceLength := s.p.Info().Length() + offset

	switch whence {
	case io.SeekStart:
		_, err := s.r.Seek(pieceOffset+offset, io.SeekStart)
		if err != nil {
			return 0, err
		}
		return offset, nil
	case io.SeekCurrent:
		n, err := s.r.Seek(offset, io.SeekCurrent)
		if err != nil {
			return 0, err
		}
		return n - pieceOffset, nil
	case io.SeekEnd:
		n, err := s.r.Seek(pieceOffset+pieceLength+offset, io.SeekStart)
		if err != nil {
			return 0, err
		}
		return n - pieceOffset, nil
	}
	return 0, nil
}
