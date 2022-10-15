package services

import (
	"crypto/sha1"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/anacrolix/missinggo/v2"
	"github.com/edsrzf/mmap-go"

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
func (s *mmapClientImpl) distributeByHash(dirs []string, hash string) (string, error) {
	sort.Strings(dirs)
	hex := fmt.Sprintf("%x", sha1.Sum([]byte(hash)))[0:5]
	num64, err := strconv.ParseInt(hex, 16, 64)
	if err != nil {
		return "", errors.Wrapf(err, "failed to parse hex from hex=%v infohash=%v", hex, hash)
	}
	num := int(num64 * 1000)
	total := 1048575 * 1000
	interval := total / len(dirs)
	for i := 0; i < len(dirs); i++ {
		if num < (i+1)*interval {
			return dirs[i], nil
		}
	}
	return "", errors.Wrapf(err, "failed to distribute infohash=%v", hash)
}

func (s *mmapClientImpl) getDir(location string, hash string) (string, error) {
	if strings.HasSuffix(location, "*") {
		prefix := strings.TrimSuffix(location, "*")
		dir, lp := path.Split(prefix)
		if dir == "" {
			dir = "."
		}
		files, err := ioutil.ReadDir(dir)
		if err != nil {
			return "", err
		}
		dirs := []string{}
		for _, f := range files {
			if f.IsDir() && strings.HasPrefix(f.Name(), lp) {
				dirs = append(dirs, f.Name())
			}
		}
		if len(dirs) == 0 {
			return prefix + "/" + hash, nil
		} else if len(dirs) == 1 {
			return dir + "/" + dirs[0] + "/" + hash, nil
		} else {
			d, err := s.distributeByHash(dirs, hash)
			if err != nil {
				return "", err
			}
			return dir + "/" + d + "/" + hash, nil
		}
	} else {
		return location + "/" + hash, nil
	}
}

func (s *mmapClientImpl) OpenTorrent(info *metainfo.Info, infoHash metainfo.Hash) (_ storage.TorrentImpl, err error) {
	dir, err := s.getDir(s.baseDir, infoHash.HexString())
	if err != nil {
		return
	}
	span, err := mMapTorrent(info, dir)
	t := &mmapTorrentStorage{
		infoHash: infoHash,
		span:     span,
		pc:       pieceCompletionForDir(dir),
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
		fileName := filepath.Join(location, safeName)
		var mm mmap.MMap
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

func mmapFile(name string, size int64) (ret mmap.MMap, err error) {
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
	defer file.Close()
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
	if size == 0 {
		// Can't mmap() regions with length 0.
		return
	}
	intLen := int(size)
	if int64(intLen) != size {
		err = errors.New("size too large for system")
		return
	}
	ret, err = mmap.MapRegion(file, intLen, mmap.RDWR, 0, 0)
	if err != nil {
		err = fmt.Errorf("error mapping region: %s", err)
		return
	}
	if int64(len(ret)) != size {
		panic(len(ret))
	}
	return
}

func pieceCompletionForDir(dir string) (ret storage.PieceCompletion) {
	ret, err := storage.NewDefaultPieceCompletionForDir(dir)
	if err != nil {
		log.Printf("couldn't open piece completion db in %q: %s", dir, err)
		ret = storage.NewMapPieceCompletion()
	}
	return
}
