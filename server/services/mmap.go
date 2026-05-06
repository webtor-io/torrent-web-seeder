package services

import (
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"io"
	stdlog "log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/missinggo/v2"
	"github.com/anacrolix/torrent"
	"github.com/edsrzf/mmap-go"

	"github.com/anacrolix/torrent/metainfo"
	mmapSpan "github.com/anacrolix/torrent/mmap-span"
	"github.com/anacrolix/torrent/storage"

	log "github.com/sirupsen/logrus"
)

// evictShards bounds the number of RWMutexes used to serialize piece reads
// against eviction. Power of two for cheap sharding via bitmask.
const evictShards = 1024

type mmapClientImpl struct {
	baseDir string
	budget  int64
	cl      *torrent.Client // set after torrent.NewClient(), used for eviction VerifyData
}

// NewMMap creates a mmap-based storage backend.
// budget is the per-torrent cache budget in bytes (0 = unlimited, no eviction).
func NewMMap(baseDir string, budget int64) *mmapClientImpl {
	if budget > 0 {
		promCacheBudget.Set(float64(budget))
	}
	return &mmapClientImpl{
		baseDir: baseDir,
		budget:  budget,
	}
}

// SetClient wires the torrent client reference for piece eviction.
// Called once after torrent.NewClient().
func (s *mmapClientImpl) SetClient(cl *torrent.Client) {
	s.cl = cl
}

func (s *mmapClientImpl) OpenTorrent(_ context.Context, info *metainfo.Info, infoHash metainfo.Hash) (_ storage.TorrentImpl, err error) {
	dir, err := GetDir(s.baseDir, infoHash.HexString())
	if err != nil {
		return
	}
	span, files, fileLens, mmaps, err := mMapTorrent(info, dir)
	if err != nil {
		return
	}
	pc := pieceCompletionForDir(dir, info, infoHash)

	// Only enable LRU eviction if the torrent is larger than the cache budget.
	// Small torrents fit entirely in cache — no eviction overhead needed.
	evictionEnabled := s.budget > 0 && info.TotalLength() > s.budget

	t := &mmapTorrentStorage{
		infoHash: infoHash,
		span:     span,
		pc:       pc,
		info:     info,
		files:    files,
		fileLens: fileLens,
		mmaps:    mmaps,
		closeCh:  make(chan struct{}),
		cl:       s.cl,
	}

	if evictionEnabled {
		lru := NewPieceLRU(s.budget)
		// Protect pieces belonging to completed files from eviction (first pass).
		if pci, ok := pc.(*pieceCompletion); ok {
			lru.SetProtectedFunc(func(index int) bool {
				return pci.completions.IsPieceInCompletedFile(index)
			})
		}
		recoverLRU(lru, pc, info, infoHash)
		t.lru = lru
		t.verifyCh = make(chan int, 256)

		if lru.Used() > s.budget {
			log.Infof("cache over budget on recovery (%d > %d), evicting", lru.Used(), s.budget)
			t.evictOverBudget()
		}
		t.startEvictionSweep()
		log.Infof("eviction enabled for torrent %s (size=%d > budget=%d)",
			infoHash.HexString(), info.TotalLength(), s.budget)
	}

	// Hint the kernel that mmap'd regions will be read sequentially (streaming).
	// This enables aggressive readahead and proactive page reclamation after reads.
	for _, m := range mmaps {
		if m != nil {
			_ = madviseSequential(m)
		}
	}

	impl := storage.TorrentImpl{
		Piece: t.Piece,
		Close: t.Close,
	}
	// Note: we intentionally do NOT set impl.Capacity here.
	// TorrentCapacity with RemainingBudget=0 causes anacrolix to stop requesting
	// pieces entirely, which hangs downloads. Eviction is enforced synchronously
	// in MarkComplete and via background sweep — Capacity is not needed.
	return impl, nil
}

// recoverLRU populates the LRU tracker with pieces already marked complete in SQLite.
func recoverLRU(lru *PieceLRU, pc storage.PieceCompletion, info *metainfo.Info, infoHash metainfo.Hash) {
	completePieces := make(map[int]int64)
	for i := 0; i < info.NumPieces(); i++ {
		pk := metainfo.PieceKey{InfoHash: infoHash, Index: i}
		c, err := pc.Get(pk)
		if err != nil {
			continue
		}
		if c.Ok && c.Complete {
			completePieces[i] = info.Piece(i).Length()
		}
	}
	if len(completePieces) > 0 {
		lru.Recover(completePieces)
		log.Infof("recovered %d complete pieces (%d bytes) for LRU",
			len(completePieces), lru.Used())
		promCacheBytesUsed.Add(float64(lru.Used()))
		promCachePieceCount.Add(float64(len(completePieces)))
	}
}

func (s *mmapClientImpl) Close() error {
	return nil
}

type mmapTorrentStorage struct {
	infoHash metainfo.Hash
	span     *mmapSpan.MMapSpan
	pc       storage.PieceCompletion
	lru      *PieceLRU
	info     *metainfo.Info
	files    []*os.File  // file handles for hole-punching
	fileLens []int64     // file lengths for piece→file mapping
	mmaps    []mmap.MMap // raw mmap regions per file, for madvise after eviction
	closeCh  chan struct{}
	cl       *torrent.Client // for VerifyData on eviction
	verifyCh chan int        // evicted piece indices queued for VerifyData
	// evictMu serializes piece reads against eviction. Sharded by piece
	// index so eviction of one piece does not block reads of unrelated
	// pieces. ReadAt takes RLock; evictPiece takes Lock — guaranteeing
	// the mmap region is not punch-holed mid-read, which would otherwise
	// hand zero-filled bytes to the HTTP client.
	evictMu [evictShards]sync.RWMutex
}

// pieceLock returns the RWMutex shard for a given piece index.
func (ts *mmapTorrentStorage) pieceLock(idx int) *sync.RWMutex {
	return &ts.evictMu[uint(idx)&(evictShards-1)]
}

func (ts *mmapTorrentStorage) Piece(p metainfo.Piece) storage.PieceImpl {
	return mmapStoragePiece{
		t:             ts,
		p:             p,
		sectionReader: io.NewSectionReader(ts.span, p.Offset(), p.Length()),
		sectionWriter: missinggo.NewSectionWriter(ts.span, p.Offset(), p.Length()),
	}
}

// startEvictionSweep runs a periodic background eviction sweep.
// Also processes the verify queue — calling VerifyData on evicted pieces
// to notify anacrolix that they need re-downloading.
func (ts *mmapTorrentStorage) startEvictionSweep() {
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ts.closeCh:
				return
			case idx := <-ts.verifyCh:
				ts.verifyPiece(idx)
			case <-ticker.C:
				if ts.lru.Used() > ts.lru.budget && ts.lru.budget > 0 {
					ts.evictOverBudget()
				}
			}
		}
	}()
}

// verifyPiece calls VerifyData on a piece to notify anacrolix that it's no longer valid.
func (ts *mmapTorrentStorage) verifyPiece(idx int) {
	if ts.cl == nil {
		return
	}
	for _, t := range ts.cl.Torrents() {
		if t.InfoHash() == ts.infoHash {
			t.Piece(idx).VerifyData()
			return
		}
	}
}

func (ts *mmapTorrentStorage) Close() error {
	close(ts.closeCh)
	if ts.lru != nil {
		promCacheBytesUsed.Sub(float64(ts.lru.Used()))
		ts.lru.mu.Lock()
		promCachePieceCount.Sub(float64(len(ts.lru.entries)))
		ts.lru.mu.Unlock()
	}
	// Advise the kernel to drop all mmap'd pages before unmapping.
	// This ensures immediate RSS release when a torrent is dropped,
	// rather than waiting for the kernel to lazily reclaim pages.
	for _, m := range ts.mmaps {
		if m != nil {
			_ = madviseEvict(m)
		}
	}
	return ts.span.Close()
}

type mmapStoragePiece struct {
	t             *mmapTorrentStorage
	p             metainfo.Piece
	sectionReader *io.SectionReader
	sectionWriter *missinggo.SectionWriter
}

func (me mmapStoragePiece) ReadAt(b []byte, off int64) (int, error) {
	// Hold the piece shard for read so an in-flight evictPiece cannot
	// punch holes in the mmap region while we are copying out of it.
	mu := me.t.pieceLock(me.p.Index())
	mu.RLock()
	defer mu.RUnlock()

	// Defense-in-depth: between the caller's Completion() check and now,
	// the piece may have been evicted (pc.Set(false) is the first thing
	// evictPiece does). Refuse to serve a piece whose backing bytes may
	// already be holes — anacrolix re-fetches it via BitTorrent instead
	// of streaming zeros to the client.
	if me.t.lru != nil {
		c, err := me.t.pc.Get(me.pieceKey())
		if err == nil && c.Ok && !c.Complete {
			return 0, io.ErrUnexpectedEOF
		}
	}

	if me.t.lru != nil {
		me.t.lru.Touch(me.p.Index())
	}
	n, err := me.sectionReader.ReadAt(b, off)
	if n > 0 && me.t.lru != nil {
		me.t.madviseSpanRange(me.p.Offset()+off, int64(n))
	}
	return n, err
}

func (me mmapStoragePiece) WriteAt(b []byte, off int64) (int, error) {
	return me.sectionWriter.WriteAt(b, off)
}

func (me mmapStoragePiece) Flush() error {
	return me.t.span.Flush()
}

func (me mmapStoragePiece) pieceKey() metainfo.PieceKey {
	return metainfo.PieceKey{InfoHash: me.t.infoHash, Index: me.p.Index()}
}

func (sp mmapStoragePiece) Completion() storage.Completion {
	c, err := sp.t.pc.Get(sp.pieceKey())
	if err != nil {
		panic(err)
	}
	return c
}

func (sp mmapStoragePiece) MarkComplete() error {
	err := sp.t.pc.Set(sp.pieceKey(), true)
	if err != nil {
		return err
	}
	// The piece has been fully written and verified. Drop the mmap pages —
	// dirty pages will be written back by the kernel asynchronously, and
	// clean pages are freed immediately. This prevents downloaded pieces
	// from accumulating in cgroup memory.
	if sp.t.lru != nil {
		sp.t.madviseSpanRange(sp.p.Offset(), sp.p.Length())
	}
	if sp.t.lru != nil {
		toEvict := sp.t.lru.Add(sp.p.Index(), sp.p.Length())
		promCacheBytesUsed.Add(float64(sp.p.Length()))
		promCachePieceCount.Inc()
		for _, idx := range toEvict {
			sp.t.evictPiece(idx)
		}
	}
	return nil
}

func (sp mmapStoragePiece) MarkNotComplete() error {
	return sp.t.pc.Set(sp.pieceKey(), false)
}

// madviseSpanRange advises the kernel to drop pages for a byte range in the
// concatenated mmap span. Used after ReadAt to free page cache for data that
// has already been copied into a userspace buffer.
func (ts *mmapTorrentStorage) madviseSpanRange(spanOff, length int64) {
	end := spanOff + length
	fileOff := int64(0)
	for i, fLen := range ts.fileLens {
		fEnd := fileOff + fLen
		if end > fileOff && spanOff < fEnd {
			regStart := max(spanOff, fileOff) - fileOff
			regEnd := min(end, fEnd) - fileOff
			if i < len(ts.mmaps) && ts.mmaps[i] != nil && regEnd <= int64(len(ts.mmaps[i])) {
				_ = madviseEvict(ts.mmaps[i][regStart:regEnd])
			}
		}
		if fileOff >= end {
			break
		}
		fileOff = fEnd
	}
}

// fileRegion describes a contiguous region within a single torrent file.
type fileRegion struct {
	fileIndex int
	offset    int64
	length    int64
}

// pieceFileRegions computes which file regions a piece covers.
// A piece may span multiple files in a multi-file torrent.
func (ts *mmapTorrentStorage) pieceFileRegions(p metainfo.Piece) []fileRegion {
	pieceOff := p.Offset()
	pieceLen := p.Length()
	pEnd := pieceOff + pieceLen
	var regions []fileRegion
	fileOff := int64(0)
	for i, fLen := range ts.fileLens {
		fEnd := fileOff + fLen
		if pEnd > fileOff && pieceOff < fEnd {
			regStart := max(pieceOff, fileOff) - fileOff
			regLen := min(pEnd, fEnd) - max(pieceOff, fileOff)
			regions = append(regions, fileRegion{
				fileIndex: i,
				offset:    regStart,
				length:    regLen,
			})
		}
		fileOff = fEnd
	}
	return regions
}

// evictOverBudget runs eviction until cache usage is within budget.
// Called once after recovery if the recovered state exceeds the budget.
func (ts *mmapTorrentStorage) evictOverBudget() {
	ts.lru.mu.Lock()
	toEvict := ts.lru.computeEvictions()
	ts.lru.mu.Unlock()
	for _, idx := range toEvict {
		ts.evictPiece(idx)
	}
}

// evictPiece removes a piece from cache by punching holes in the mmap'd files
// and marking it as incomplete. Holds the piece shard's eviction lock for the
// entire mutating section so concurrent ReadAt calls cannot observe the
// transient state where mmap pages have been zeroed but completion still
// reports the piece as available.
func (ts *mmapTorrentStorage) evictPiece(idx int) {
	piece := ts.info.Piece(idx)
	pk := metainfo.PieceKey{InfoHash: ts.infoHash, Index: idx}

	mu := ts.pieceLock(idx)
	mu.Lock()

	if err := ts.pc.Set(pk, false); err != nil {
		mu.Unlock()
		log.WithError(err).Errorf("failed to mark piece %d incomplete during eviction", idx)
		return
	}
	ts.uncompleteAffectedFiles(idx)

	for _, region := range ts.pieceFileRegions(piece) {
		if region.fileIndex >= len(ts.files) || ts.files[region.fileIndex] == nil {
			continue
		}
		if err := punchHole(ts.files[region.fileIndex], region.offset, region.length); err != nil {
			log.WithError(err).Errorf("failed to punch hole for piece %d in file %d", idx, region.fileIndex)
		}
		if region.fileIndex < len(ts.mmaps) && ts.mmaps[region.fileIndex] != nil {
			end := region.offset + region.length
			if end > int64(len(ts.mmaps[region.fileIndex])) {
				end = int64(len(ts.mmaps[region.fileIndex]))
			}
			if region.offset < end {
				if err := madviseEvict(ts.mmaps[region.fileIndex][region.offset:end]); err != nil {
					log.WithError(err).Errorf("madvise failed for piece %d in file %d", idx, region.fileIndex)
				}
			}
		}
	}

	prevUsed := ts.lru.Used()
	ts.lru.Remove(idx)
	freedBytes := prevUsed - ts.lru.Used()
	promCacheBytesUsed.Sub(float64(freedBytes))
	promCachePieceCount.Dec()
	promCacheEvictions.Inc()

	mu.Unlock()

	log.Infof("evicted piece %d, freed %d bytes, used=%d budget=%d",
		idx, freedBytes, ts.lru.Used(), ts.lru.budget)

	// Notify anacrolix asynchronously — calling VerifyData synchronously
	// from MarkComplete's call chain (the LRU eviction trigger) can
	// deadlock on anacrolix's internal locks. Correctness no longer
	// depends on this signal because pc.Set(false) above plus the ReadAt
	// completion recheck under RLock already prevent zero-filled reads.
	select {
	case ts.verifyCh <- idx:
	default:
	}
}

// uncompleteAffectedFiles finds files that include the given piece index
// and removes them from the file_completion table.
func (ts *mmapTorrentStorage) uncompleteAffectedFiles(pieceIndex int) {
	var affectedFiles []string
	if len(ts.info.Files) == 0 {
		// Single-file torrent.
		affectedFiles = append(affectedFiles, ts.info.Name)
	} else {
		offset := 0
		for _, f := range ts.info.Files {
			path := ts.info.Name + "/" + strings.Join(f.Path, "/")
			startPiece := offset / int(ts.info.PieceLength)
			endPiece := (offset + int(f.Length)) / int(ts.info.PieceLength)
			offset += int(f.Length)
			if pieceIndex >= startPiece && pieceIndex <= endPiece {
				affectedFiles = append(affectedFiles, path)
			}
		}
	}
	if len(affectedFiles) == 0 {
		return
	}
	type fileUncompleter interface {
		UncompleteFiles(paths []string) error
	}
	if fu, ok := ts.pc.(fileUncompleter); ok {
		if err := fu.UncompleteFiles(affectedFiles); err != nil {
			log.WithError(err).Error("failed to uncomplete files during eviction")
		}
	}
}

func mMapTorrent(md *metainfo.Info, location string) (mms *mmapSpan.MMapSpan, files []*os.File, fileLens []int64, mmaps []mmap.MMap, err error) {
	var mMaps []FileMapping
	defer func() {
		if err != nil {
			for _, mm := range mMaps {
				err = errors.Join(err, mm.Unmap())
			}
			files = nil
			fileLens = nil
			mmaps = nil
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
		var f *os.File
		mm, f, err = mmapFile(fileName, miFile.Length)
		if err != nil {
			err = fmt.Errorf("file %q: %w", miFile.DisplayPath(md), err)
			return
		}
		mMaps = append(mMaps, mm)
		files = append(files, f)
		fileLens = append(fileLens, miFile.Length)
		mmaps = append(mmaps, mm.Bytes())
	}
	return mmapSpan.New(mMaps, md.FileSegmentsIndex()), files, fileLens, mmaps, nil
}

func mmapFile(name string, size int64) (_ FileMapping, file *os.File, err error) {
	dir := filepath.Dir(name)
	err = os.MkdirAll(dir, 0o750)
	if err != nil {
		err = fmt.Errorf("making directory %q: %s", dir, err)
		return
	}
	file, err = os.OpenFile(name, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return
	}
	defer func() {
		if err != nil {
			_ = file.Close()
			file = nil
		}
	}()
	var fi os.FileInfo
	fi, err = file.Stat()
	if err != nil {
		return
	}
	if fi.Size() < size {
		err = file.Truncate(size)
		if err != nil {
			return
		}
	}
	mapping, mapErr := func() (ret mmapWithFile, err error) {
		ret.f = file
		if size == 0 {
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
	return mapping, file, mapErr
}

// WrapFileMapping combines a mmapped region and file into a storage Mmap abstraction.
func WrapFileMapping(region mmap.MMap, file *os.File) FileMapping {
	return mmapWithFile{
		f:    file,
		mmap: region,
	}
}

type FileMapping = mmapSpan.Mmap

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
		stdlog.Printf("couldn't open piece completion db in %q: %s", dir, err)
		ret = storage.NewMapPieceCompletion()
	}
	return
}
