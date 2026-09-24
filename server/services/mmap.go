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
	"sync/atomic"
	"time"

	"github.com/anacrolix/missinggo/v2"
	"github.com/anacrolix/torrent"
	"github.com/edsrzf/mmap-go"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"

	log "github.com/sirupsen/logrus"
)

// evictShards bounds the number of RWMutexes used to serialize piece reads
// against eviction. Power of two for cheap sharding via bitmask.
const evictShards = 1024

type mmapClientImpl struct {
	baseDir   string
	events    *CacheEvents // nil: nothing is published
	budget    int64
	fileCache FileCacheConfig
	cl        *torrent.Client // set after torrent.NewClient(), used for eviction VerifyData
}

// NewMMap creates a mmap-based storage backend.
// budget is the per-torrent cache budget in bytes (0 = unlimited, no eviction).
// fileCache bounds how many of a torrent's files are open at once.
func NewMMap(baseDir string, budget int64, fileCache FileCacheConfig) *mmapClientImpl {
	if budget > 0 {
		promCacheBudget.Set(float64(budget))
	}
	return &mmapClientImpl{
		baseDir:   baseDir,
		budget:    budget,
		fileCache: fileCache,
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
	files, err := torrentSpanFiles(info, dir)
	if err != nil {
		return
	}
	// Nothing is opened here: files open on first touch and at most
	// fileCache.MaxOpen stay open (see lazySpan). OpenTorrent runs under the
	// anacrolix client lock, so its cost is paid by every other request on
	// the pod — with 184k files the eager open took ~7 s and failed anyway.
	span := newLazySpan(files, s.fileCache)
	// The completion db lives in the torrent's dir, which GetDir only names:
	// the span creates it on the first write, after this point. The first
	// open of a torrent on a pod therefore failed with SQLITE_CANTOPEN and
	// fell back to an in-memory completion (8.9k of 20.4k adds on
	// 2026-09-23), so nothing that session downloaded was recorded.
	// pieceCompletion.Get reads a missing row as "never downloaded", which
	// holds only if every session is recorded; and the fallback knows no
	// piece at all, so the library hashes the whole torrent on add.
	if err = os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	pc := pieceCompletionForDir(dir, info, infoHash, s.events)

	// Only enable LRU eviction if the torrent is larger than the cache budget.
	// Small torrents fit entirely in cache — no eviction overhead needed.
	evictionEnabled := s.budget > 0 && info.TotalLength() > s.budget

	t := &mmapTorrentStorage{
		infoHash: infoHash,
		span:     span,
		pc:       pc,
		info:     info,
		closeCh:  make(chan struct{}),
		cl:       s.cl,
	}

	if evictionEnabled {
		t.evicted = make([]atomic.Bool, info.NumPieces())
		lru := NewPieceLRU(s.budget)
		// Protect pieces belonging to completed files from eviction (first pass).
		if pci, ok := pc.(*pieceCompletion); ok {
			lru.SetProtectedFunc(func(index int) bool {
				return pci.completions.IsPieceInCompletedFile(index)
			})
		}
		recoverLRU(lru, pc, info, infoHash)
		t.lru = lru

		if lru.Used() > s.budget {
			log.Infof("cache over budget on recovery (%d > %d), evicting", lru.Used(), s.budget)
			t.evictOverBudget()
		}
		// Only now may evictions talk to anacrolix — see the `attached`
		// field comment for why this must come after evictOverBudget.
		t.attached.Store(true)
		t.startEvictionSweep()
		log.Infof("eviction enabled for torrent %s (size=%d > budget=%d)",
			infoHash.HexString(), info.TotalLength(), s.budget)
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
	span     *lazySpan // files opened on demand; descriptors and mappings for eviction live here
	pc       storage.PieceCompletion
	lru      *PieceLRU
	info     *metainfo.Info
	closeCh  chan struct{}
	cl       *torrent.Client // for completion refresh on eviction
	// evicted[i] is true while piece i's bytes are a punched hole. Set
	// before the hole is punched, cleared when the piece is written again.
	//
	// This is the correctness guard, and it is deliberately NOT derived
	// from pc completion. A pc-based guard is what a13efd6 had to remove:
	// after a re-download anacrolix hashes the piece BEFORE MarkComplete,
	// so pc still reads incomplete, the guard refused the verification
	// read, the hash failed, and any once-evicted piece became permanently
	// un-redownloadable. "Has a hole" and "is not marked complete" are
	// different facts; only the first one may refuse a read.
	evicted []atomic.Bool
	// attached gates the anacrolix notification in refreshCompletion.
	//
	// OpenTorrent runs INSIDE Torrent.setInfo, which anacrolix calls with
	// the client lock held (setInfoBytesLocked). The eviction that
	// OpenTorrent may trigger for a recovered over-budget torrent would
	// therefore call RefreshCompletionFromStorage — which takes that same
	// lock — and self-deadlock on the first oversized torrent to restart.
	//
	// Skipping the notification there is not a compromise: anacrolix runs
	// onSetInfo immediately after OpenTorrent returns, and that calls
	// setInitialPieceCompletionFromStorage for every piece, reading the
	// completion store we have just updated. The bitmap ends up correct
	// without us saying anything.
	attached atomic.Bool
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

// ErrPieceEvicted is returned by ReadAt for a piece whose bytes have been
// dropped from the cache. It is a "come back after re-downloading" answer,
// not a failure — but it must be an error rather than zeroes, because
// zeroes are indistinguishable from data.
var ErrPieceEvicted = errors.New("piece evicted from cache")

// isEvicted reports whether piece idx is currently a punched hole. Eviction
// is only ever enabled for torrents larger than the cache budget, so the
// slice is nil (and every piece present) for the common small torrent.
func (ts *mmapTorrentStorage) isEvicted(idx int) bool {
	if ts.evicted == nil || idx < 0 || idx >= len(ts.evicted) {
		return false
	}
	return ts.evicted[idx].Load()
}

// setEvicted marks (or unmarks) piece idx as a punched hole.
func (ts *mmapTorrentStorage) setEvicted(idx int, v bool) {
	if ts.evicted == nil || idx < 0 || idx >= len(ts.evicted) {
		return
	}
	ts.evicted[idx].Store(v)
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
//
// It no longer drains a verify queue: evictPiece notifies anacrolix inline
// now that the notification is cheap (a completion re-read, not a re-hash).
func (ts *mmapTorrentStorage) startEvictionSweep() {
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ts.closeCh:
				return
			case <-ticker.C:
				if ts.lru.Used() > ts.lru.budget && ts.lru.budget > 0 {
					ts.evictOverBudget()
				}
			}
		}
	}()
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
	ts.span.evictPages()
	// Close the piece-completion store: releases its background goroutine,
	// closes the SQLite handle backing .torrent.db (one FD + many in-memory
	// prepared statements), and drops the metainfo reference held inside
	// the goroutine closure. Without this, every dropped torrent leaks ~1
	// goroutine + 1 SQLite conn + the full piece/file map for the lifetime
	// of the pod — a 3-day pod accumulates thousands and OOMs.
	if ts.pc != nil {
		_ = ts.pc.Close()
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
	// The RLock + evictPiece's Lock are sufficient on their own to
	// prevent reading from a region in the middle of being punched.
	//
	// We deliberately do NOT short-circuit on pc.Get reporting !Complete.
	// That check used to live here and had to go (a13efd6): it also fired
	// during anacrolix's hashPiece verification ReadAt for a piece that had
	// just been re-downloaded after eviction — piece_completion still says
	// complete=0, because MarkComplete only fires AFTER the hash passes —
	// so the verification read returned 0 bytes and the piece could never
	// be re-acquired. Any piece evicted once became permanently
	// un-redownloadable.
	//
	// The guard below is the correct version of that idea. It keys on
	// "we punched a hole in this piece", not on "this piece is not marked
	// complete" — the two coincided in the old check, which is why it
	// caught the hasher. A re-download clears the flag on its first
	// WriteAt, long before the hash reads.
	mu := me.t.pieceLock(me.p.Index())
	mu.RLock()
	defer mu.RUnlock()

	// Refuse a piece whose bytes are currently a hole. Reading a hole
	// SUCCEEDS and yields zeroes, so without this every caller — anacrolix's
	// reader included — takes the zeroes as real data: n == len(b),
	// err == nil, so none of the reader's recovery paths
	// (clearStorageReader, updatePieceCompletion, retry) even run. The
	// client gets HTTP 200, a correct Content-Length, and a zero-filled
	// hole in the middle of the file, with nothing logged anywhere.
	//
	// An explicit error is what makes that recoverable: anacrolix retries,
	// resyncs completion from storage, and re-requests the piece.
	if me.t.isEvicted(me.p.Index()) {
		return 0, ErrPieceEvicted
	}

	if me.t.lru != nil {
		me.t.lru.Touch(me.p.Index())
	}
	n, err := me.sectionReader.ReadAt(b, off)
	if n > 0 && me.t.lru != nil {
		me.t.madviseSpanRange(me.p.Offset()+off, int64(n))
	}
	if err != nil && (err != io.EOF || n == 0) && !errors.Is(err, ErrSpanClosed) {
		// ErrSpanClosed is not logged: anacrolix sweeps every piece with a
		// 32 KB read right after Drop (1,586 lines in two seconds for one
		// warm torrent, 2026-09-21); those reads have no reader behind them
		// and say nothing new. A closed span under an HTTP reader shows up
		// as "reading from closed torrent" and the recovered panic instead.
		// anacrolix logs storage read errors to a logger this service
		// discards, and a read that returns nothing twice ends in a panic
		// inside its reader (updatePieceCompletion, "0 N"); the error
		// itself was invisible — 2026-09-21. An EOF with zero bytes is
		// such an answer too (readOnceAt treats n == 0 as failure), so it
		// is logged; a short read at the true end of a piece is not.
		log.WithError(err).WithFields(log.Fields{
			"infohash": me.t.infoHash.HexString(),
			"piece":    me.p.Index(),
			"plen":     me.p.Length(),
			"off":      off,
			"len":      len(b),
			"n":        n,
		}).Warn("piece read failed")
	}
	return n, err
}

func (me mmapStoragePiece) WriteAt(b []byte, off int64) (n int, err error) {
	// A peer's mainReadLoop can call WriteAt right while anacrolix is
	// finalizing Storage.Close(); anacrolix doesn't synchronise in-flight
	// chunk writes with storage close. lazySpan answers that with
	// ErrSpanClosed (Close waits for in-flight writes and later ones are
	// refused), which is the error the peer loop needs. The recover stays
	// as the last line: with the eager span this window was a panic that
	// took the pod down, and a mid-Close write must never do that again.
	defer func() {
		if r := recover(); r != nil {
			log.WithField("at", "mmap.WriteAt").Warnf("recovered panic (storage closing?): %v", r)
			n = 0
			err = fmt.Errorf("mmap WriteAt recovered from panic (storage closing?): %v", r)
		}
	}()
	// The piece is being written again, so it is no longer a hole and reads
	// of it must be allowed. This has to clear on the FIRST chunk write
	// rather than at MarkComplete: anacrolix hashes the piece before marking
	// it, and that hash goes through ReadAt. Clearing late is exactly the
	// trap that forced a13efd6 to delete the previous guard.
	me.t.setEvicted(me.p.Index(), false)
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
	for _, r := range ts.span.locate(spanOff, length) {
		ts.span.advise(r.fileIndex, r.offset, r.length)
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
	return ts.span.locate(p.Offset(), p.Length())
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
	// Flag first, punch second. Readers cannot currently observe the
	// difference — the shard write lock held across this whole section
	// excludes them either way, and a negative-control run with the two
	// swapped stays green. It is ordered this way so the flag is still
	// correct if the punch ever moves out from under the lock, not because
	// it closes a window today.
	ts.setEvicted(idx, true)
	ts.uncompleteAffectedFiles(idx)

	for _, region := range ts.pieceFileRegions(piece) {
		region := region
		err := ts.span.withFile(region.fileIndex, func(f *os.File, m mmap.MMap) error {
			if err := punchHole(f, region.offset, region.length); err != nil {
				log.WithError(err).Errorf("failed to punch hole for piece %d in file %d", idx, region.fileIndex)
			}
			if m != nil {
				end := min(region.offset+region.length, int64(len(m)))
				if region.offset < end {
					if err := madviseEvict(m[region.offset:end]); err != nil {
						log.WithError(err).Errorf("madvise failed for piece %d in file %d", idx, region.fileIndex)
					}
				}
			}
			return nil
		})
		if err != nil {
			log.WithError(err).Errorf("failed to open file %d to evict piece %d", region.fileIndex, idx)
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

	// Tell anacrolix the piece is gone, so it re-requests it.
	//
	// This is LIVENESS, not correctness — the evicted flag checked in ReadAt
	// is what actually stops zeroes reaching a client, and it is already set
	// above. That separation is why this can run after mu.Unlock(): the
	// refresh re-enters anacrolix (peer request updates, priority
	// recalculation) and must not do so holding a storage lock that
	// anacrolix's callbacks could need.
	//
	// pc.Set(false) alone does NOT tell anacrolix anything: reads consult
	// Torrent._completedPieces, an in-memory bitmap that only anacrolix
	// writes, and it never re-reads the completion store on its own.
	//
	// The old code queued VerifyData onto a 256-deep channel with a silent
	// `default:` drop. Two problems: VerifyData forces a full re-hash to
	// discover what we already know and blocks until it finishes, and a
	// dropped notification left the piece marked complete forever. This
	// call does neither — no hash, no queue, nothing to drop.
	ts.refreshCompletion(idx)
}

// refreshCompletion updates anacrolix's cached completion for a piece from
// the completion store, without re-hashing. Must be called with no storage
// lock held.
func (ts *mmapTorrentStorage) refreshCompletion(idx int) {
	if ts.cl == nil || !ts.attached.Load() {
		return
	}
	for _, t := range ts.cl.Torrents() {
		if t.InfoHash() == ts.infoHash {
			t.Piece(idx).RefreshCompletionFromStorage()
			return
		}
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

// torrentSpanFiles lays the torrent's files out under location as
// content/<2 hex>/<sha1 of the safe path>, the flat, content-addressed layout
// this storage has always used (a 20-deep tree is not something to mkdir
// 184k times). Nothing is opened.
func torrentSpanFiles(md *metainfo.Info, location string) ([]spanFile, error) {
	var files []spanFile
	for _, miFile := range md.UpvertedFiles() {
		safeName, err := storage.ToSafeFilePath(append([]string{md.BestName()}, miFile.BestPath()...)...)
		if err != nil {
			return nil, err
		}
		hash := sha1.Sum([]byte(safeName))
		hexHash := fmt.Sprintf("%x", hash)
		files = append(files, spanFile{
			path:   filepath.Join(location, "content", hexHash[:2], hexHash),
			length: miFile.Length,
			// The metainfo's own offset: piece-aligned in v2/hybrid torrents.
			offset: miFile.TorrentOffset,
		})
	}
	return files, nil
}

func pieceCompletionForDir(dir string, info *metainfo.Info, hash metainfo.Hash, events *CacheEvents) (ret storage.PieceCompletion) {
	ret, err := NewPieceCompletion(dir, info, hash, events)
	if err != nil {
		stdlog.Printf("couldn't open piece completion db in %q: %s", dir, err)
		ret = storage.NewMapPieceCompletion()
	}
	return
}
