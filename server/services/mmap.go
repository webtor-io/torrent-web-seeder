package services

import (
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/anacrolix/missinggo/v2"
	"github.com/edsrzf/mmap-go"

	"github.com/anacrolix/torrent/metainfo"
	mmapSpan "github.com/anacrolix/torrent/mmap-span"
	"github.com/anacrolix/torrent/storage"
)

type mmapClientImpl struct {
	baseDir string
}

func NewMMap(baseDir string) storage.ClientImplCloser {
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
	return storage.TorrentImpl{Piece: t.Piece, Close: t.Close}, err
}

func (s *mmapClientImpl) Close() error {
	return nil
}

type mmapTorrentStorage struct {
	infoHash metainfo.Hash
	span     *mmapSpan.MMapSpan
	pc       storage.PieceCompletion
}

func (ts *mmapTorrentStorage) Piece(p metainfo.Piece) storage.PieceImpl {
	return mmapStoragePiece{
		t:        ts,
		p:        p,
		ReaderAt: io.NewSectionReader(ts.span, p.Offset(), p.Length()),
		WriterAt: missinggo.NewSectionWriter(ts.span, p.Offset(), p.Length()),
	}
}

func (ts *mmapTorrentStorage) Close() error {
	return ts.span.Close()
}

type mmapStoragePiece struct {
	t *mmapTorrentStorage
	p metainfo.Piece
	io.ReaderAt
	io.WriterAt
}

var _ storage.Flusher = mmapStoragePiece{}

func (me mmapStoragePiece) Flush() error {
	// TODO: Flush just the regions of the files we care about. At least this is no worse than it
	// was previously.
	return me.t.span.Flush()
}

func (me mmapStoragePiece) pieceKey() metainfo.PieceKey {
	return metainfo.PieceKey{me.t.infoHash, me.p.Index()}
}

func (sp mmapStoragePiece) Completion() storage.Completion {
	c, err := sp.t.pc.Get(sp.pieceKey())
	if err != nil {
		panic(err)
	}
	return c
}

func (sp mmapStoragePiece) MarkComplete() error {
	return sp.t.pc.Set(sp.pieceKey(), true)
}

func (sp mmapStoragePiece) MarkNotComplete() error {
	return sp.t.pc.Set(sp.pieceKey(), false)
}

func mMapTorrent(md *metainfo.Info, location string) (mms *mmapSpan.MMapSpan, err error) {
	var mMaps []FileMapping
	defer func() {
		if err != nil {
			for _, mm := range mMaps {
				err = errors.Join(err, mm.Unmap())
			}
		}
	}()
	for _, miFile := range md.UpvertedFiles() {
		var safeName string
		safeName, err = storage.ToSafeFilePath(append([]string{md.BestName()}, miFile.BestPath()...)...)
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
			err = fmt.Errorf("file %q: %w", miFile.DisplayPath(md), err)
			return
		}
		mMaps = append(mMaps, mm)
	}
	return mmapSpan.New(mMaps, md.FileSegmentsIndex()), nil
}

func mmapFile(name string, size int64) (_ FileMapping, err error) {
	dir := filepath.Dir(name)
	err = os.MkdirAll(dir, 0o750)
	if err != nil {
		err = fmt.Errorf("making directory %q: %s", dir, err)
		return
	}
	var file *os.File
	file, err = os.OpenFile(name, os.O_CREATE|os.O_RDWR, 0o644)
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

// Combines a mmapped region and file into a storage Mmap abstraction, which handles closing the
// mmap file handle.
func WrapFileMapping(region mmap.MMap, file *os.File) FileMapping {
	return mmapWithFile{
		f:    file,
		mmap: region,
	}
}

type FileMapping = mmapSpan.Mmap

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
