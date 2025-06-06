package services

import (
	"context"
	"crypto/sha1"
	"fmt"
	"github.com/anacrolix/missinggo/v2"
	"github.com/edsrzf/mmap-go"
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/mmap_span"
	"github.com/anacrolix/torrent/storage"
	"github.com/pkg/errors"
)

type mmapClientImpl struct {
	baseDir string
}

func NewMMap(baseDir string) *mmapClientImpl {
	return &mmapClientImpl{
		baseDir: baseDir,
	}
}

func (s *mmapClientImpl) OpenTorrent(_ context.Context, info *metainfo.Info, infoHash metainfo.Hash) (_ storage.TorrentImpl, err error) {
	dir, err := GetDir(s.baseDir, infoHash.HexString())
	if err != nil {
		return
	}
	span, err := mMapTorrent(info, dir)
	t := &mmapTorrentStorage{
		infoHash: infoHash,
		span:     span,
		pc:       pieceCompletionForDir(dir, info, infoHash),
	}
	return storage.TorrentImpl{Piece: t.Piece, Close: t.Close, Flush: t.Flush}, err
}

func (s *mmapClientImpl) Close() error {
	return nil
}

type mmapTorrentStorage struct {
	infoHash metainfo.Hash
	span     *mmap_span.MMapSpan
	pc       storage.PieceCompletion
}

func (ts *mmapTorrentStorage) Piece(p metainfo.Piece) storage.PieceImpl {
	return mmapStoragePiece{
		pc:       ts.pc,
		p:        p,
		ih:       ts.infoHash,
		ReaderAt: io.NewSectionReader(ts.span, p.Offset(), p.Length()),
		WriterAt: missinggo.NewSectionWriter(ts.span, p.Offset(), p.Length()),
	}
}

func (ts *mmapTorrentStorage) Close() error {
	ts.pc.Close()
	errs := ts.span.Close()
	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}
func (ts *mmapTorrentStorage) Flush() error {
	errs := ts.span.Flush()
	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}

type mmapStoragePiece struct {
	pc storage.PieceCompletionGetSetter
	p  metainfo.Piece
	ih metainfo.Hash
	io.ReaderAt
	io.WriterAt
}

func (me mmapStoragePiece) pieceKey() metainfo.PieceKey {
	return metainfo.PieceKey{me.ih, me.p.Index()}
}

func (sp mmapStoragePiece) Completion() storage.Completion {
	c, err := sp.pc.Get(sp.pieceKey())
	if err != nil {
		panic(err)
	}
	return c
}

func (sp mmapStoragePiece) MarkComplete() error {
	sp.pc.Set(sp.pieceKey(), true)
	return nil
}

func (sp mmapStoragePiece) MarkNotComplete() error {
	sp.pc.Set(sp.pieceKey(), false)
	return nil
}

func mMapTorrent(md *metainfo.Info, location string) (mms *mmap_span.MMapSpan, err error) {
	mms = &mmap_span.MMapSpan{}
	defer func() {
		if err != nil {
			mms.Close()
		}
	}()
	for _, miFile := range md.UpvertedFiles() {
		var safeName string
		safeName, err = storage.ToSafeFilePath(append([]string{md.Name}, miFile.Path...)...)
		if err != nil {
			return
		}
		hash := sha1.Sum([]byte(safeName))
		hexHash := fmt.Sprintf("%x", hash)
		subPath := hexHash[:2]
		fileName := filepath.Join(location, "content", subPath, hexHash)
		var mm FileMapping
		mm, err = mmapFile(fileName, miFile.Length)
		if err != nil {
			err = fmt.Errorf("file %q: %s", miFile.DisplayPath(md), err)
			return
		}
		if mm != nil {
			mms.Append(mm)
		}
	}
	mms.InitIndex()
	return
}

func mmapFile(name string, size int64) (_ FileMapping, err error) {
	dir := filepath.Dir(name)
	err = os.MkdirAll(dir, 0o750)
	if err != nil {
		err = fmt.Errorf("making directory %q: %s", dir, err)
		return
	}
	var file *os.File
	file, err = os.OpenFile(name, os.O_CREATE|os.O_RDWR, 0o666)
	if err != nil {
		return
	}
	defer func() {
		if err != nil {
			_ = file.Close()
		}
	}()
	var fi os.FileInfo
	fi, err = file.Stat()
	if err != nil {
		return
	}
	if fi.Size() < size {
		// I think this is necessary on HFS+. Maybe Linux will SIGBUS too if
		// you overmap a file but I'm not sure.
		err = file.Truncate(size)
		if err != nil {
			return
		}
	}
	return func() (ret mmapWithFile, err error) {
		ret.f = file
		if size == 0 {
			// Can't mmap() regions with length 0.
			return
		}
		intLen := int(size)
		if int64(intLen) != size {
			err = errors.New("size too large for system")
			return
		}
		ret.mmap, err = mmap.MapRegion(file, intLen, mmap.RDWR, 0, 0)
		if err != nil {
			err = fmt.Errorf("error mapping region: %s", err)
			return
		}
		if int64(len(ret.mmap)) != size {
			panic(len(ret.mmap))
		}
		return
	}()
}

type FileMapping = mmap_span.Mmap

// Handles closing the mmap's file handle (needed for Windows). Could be implemented differently by
// OS.
type mmapWithFile struct {
	f    *os.File
	mmap mmap.MMap
}

func (m mmapWithFile) Flush() error {
	return m.mmap.Flush()
}

func (m mmapWithFile) Unmap() (err error) {
	if m.mmap != nil {
		err = m.mmap.Unmap()
	}
	fileErr := m.f.Close()
	if err == nil {
		err = fileErr
	}
	return
}

func (m mmapWithFile) Bytes() []byte {
	if m.mmap == nil {
		return nil
	}
	return m.mmap
}

func pieceCompletionForDir(dir string, info *metainfo.Info, hash metainfo.Hash) (ret storage.PieceCompletion) {
	ret, err := NewPieceCompletion(dir, info, hash)
	if err != nil {
		log.Printf("couldn't open piece completion db in %q: %s", dir, err)
		ret = storage.NewMapPieceCompletion()
	}
	return
}
