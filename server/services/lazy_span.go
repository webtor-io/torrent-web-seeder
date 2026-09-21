package services

import (
	"container/list"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/edsrzf/mmap-go"
)

// lazySpan presents a torrent's files as one contiguous byte space, like
// anacrolix's MMapSpan, but opens a file only when a read or write touches it
// and keeps at most maxOpen of them open at a time.
//
// The eager span opened one os.File and one mmap per file for the life of
// the torrent. A 184k-file torrent therefore needed 184k descriptors and
// 184k mappings against 65,535 of each in the pod, could never be opened,
// and every GET retried the full open (~7 s under the client lock) — the
// 2026-09-20 "too many open files" storm. Here the cost of a torrent is
// bounded by maxOpen whatever its file count; a torrent with fewer files
// than maxOpen never evicts and behaves as before.
//
// Files shorter than mmapMin are served with pread/pwrite instead of a
// mapping: a source dump has thousands of 2 KB headers per piece, and a
// mapping per header is what the map-count limit is made of. Files longer
// than mmapMax take the same path for the opposite reason: a mapping's
// page tables are kernel memory charged to the pod, MADV_DONTNEED frees
// the pages but not the tables, and a pod with 384 GB mapped held 617 MB
// of them — a third of its limit — until the torrent was dropped (audit
// 2026-09-20: half of the OOM kills were that, not the heap). A per-file
// cap alone does not bound that: a 1627-file pack of 3.9 GiB files mapped
// 698 GiB on one pod (1.4 GiB of page tables). mmapBudget bounds the
// bytes mapped per torrent; files opened past it use pread as well, and
// the bound on page tables is ~2 MiB per GiB of budget.
//
// Locking: mu guards the open table and the LRU. An entry in use (refs > 0)
// is never evicted, so a mapping cannot be unmapped under a reader on another
// file — that would be SIGBUS, not an error. closeMu lets Close wait for
// in-flight reads and writes instead of unmapping under them.
type lazySpan struct {
	files   []spanFile
	total   int64
	maxOpen int
	mmapMin int64
	mmapMax int64
	// mmapBudget caps the bytes mapped at once; mapped is the running sum,
	// guarded by mu.
	mmapBudget int64
	mapped     int64

	closeMu sync.RWMutex
	mu      sync.Mutex
	open    map[int]*spanEntry
	lru     *list.List // front = most recently used; values are *spanEntry
	closed  bool
}

// spanFile is one torrent file: where it lives on disk, how long it is and
// where it starts in the span.
type spanFile struct {
	path   string
	length int64
	offset int64
}

type spanEntry struct {
	idx  int
	f    *os.File
	m    mmap.MMap // nil for files outside [mmapMin, mmapMax]
	refs int
	elem *list.Element
}

// FileCacheConfig bounds what one torrent may hold open.
type FileCacheConfig struct {
	// MaxOpen is the number of files kept open per torrent. Zero means the
	// default.
	MaxOpen int
	// MmapMin is the smallest file length that gets a mapping; shorter files
	// use pread/pwrite. Zero means the default.
	MmapMin int64
	// MmapMax is the largest file length that gets a mapping; longer files
	// use pread/pwrite. Zero means the default.
	MmapMax int64
	// MmapBudget is the most bytes a torrent keeps mapped at once; files
	// opened past it use pread/pwrite. Zero means the default.
	MmapBudget int64
}

const (
	defaultMaxOpen = 2048
	defaultMmapMin = 64 * 1024
	defaultMmapMax = 4 << 30
	// 8 GiB of mappings is ~16 MiB of page tables; enough for a film and its
	// neighbours, and a pack of a thousand films stays on pread.
	defaultMmapBudget = 8 << 30
)

func (c FileCacheConfig) withDefaults() FileCacheConfig {
	if c.MaxOpen <= 0 {
		c.MaxOpen = defaultMaxOpen
	}
	if c.MmapMin <= 0 {
		c.MmapMin = defaultMmapMin
	}
	if c.MmapMax <= 0 {
		c.MmapMax = defaultMmapMax
	}
	if c.MmapBudget <= 0 {
		c.MmapBudget = defaultMmapBudget
	}
	return c
}

// ErrSpanClosed is returned by reads and writes after Close.
var ErrSpanClosed = errors.New("storage span closed")

// newLazySpan takes files with their offsets already set and opens none of
// them. Files must be in offset order. offset is the file's TorrentOffset
// as the metainfo reports it, NOT the sum of the lengths before it: v2 and
// hybrid torrents align every file to a piece boundary, so there are gaps
// between files (the v1 pad files). Summing lengths shifted every file
// after the first and served the wrong bytes (2026-09-21, hybrid torrent
// 3e773d6b). Gaps read as zeros and swallow writes.
func newLazySpan(files []spanFile, cfg FileCacheConfig) *lazySpan {
	cfg = cfg.withDefaults()
	var total int64
	for i := range files {
		if i > 0 && files[i].offset < files[i-1].offset+files[i-1].length {
			panic(fmt.Sprintf("span files out of order: file %d at %d overlaps file %d ending at %d", i, files[i].offset, i-1, files[i-1].offset+files[i-1].length))
		}
		total = max(total, files[i].offset+files[i].length)
	}
	return &lazySpan{
		files:      files,
		total:      total,
		maxOpen:    cfg.MaxOpen,
		mmapMin:    cfg.MmapMin,
		mmapMax:    cfg.MmapMax,
		mmapBudget: cfg.MmapBudget,
		open:       map[int]*spanEntry{},
		lru:        list.New(),
	}
}

// Len is the span length in bytes.
func (s *lazySpan) Len() int64 { return s.total }

// fileRange returns the offset and length of file idx within the span.
func (s *lazySpan) fileRange(idx int) (offset, length int64) {
	return s.files[idx].offset, s.files[idx].length
}

// locate splits [off, off+length) into per-file regions, skipping
// zero-length files (they hold no bytes, so no region and no descriptor).
// Binary search on the file offsets: the linear scan the eager span used
// per operation is O(files), and 184k files per read is its own stall.
func (s *lazySpan) locate(off, length int64) []fileRegion {
	if length <= 0 || off >= s.total {
		return nil
	}
	end := min(off+length, s.total)
	// First file whose end is past off.
	i := sort.Search(len(s.files), func(i int) bool {
		return s.files[i].offset+s.files[i].length > off
	})
	var regions []fileRegion
	for ; i < len(s.files) && s.files[i].offset < end; i++ {
		f := s.files[i]
		if f.length == 0 {
			continue
		}
		start := max(off, f.offset) - f.offset
		stop := min(end, f.offset+f.length) - f.offset
		regions = append(regions, fileRegion{fileIndex: i, offset: start, length: stop - start})
	}
	return regions
}

// ReadAt implements io.ReaderAt over the span. Short reads at the end of the
// span return io.EOF, as io.SectionReader expects.
func (s *lazySpan) ReadAt(b []byte, off int64) (n int, err error) {
	s.closeMu.RLock()
	defer s.closeMu.RUnlock()
	if s.closed {
		return 0, ErrSpanClosed
	}
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	end := min(off+int64(len(b)), s.total)
	cur := off
	for _, r := range s.locate(off, int64(len(b))) {
		// A gap before this file (pad region of a v2 layout) reads as zeros.
		if start := s.files[r.fileIndex].offset + r.offset; start > cur {
			n += zero(b[n : n+int(start-cur)])
			cur = start
		}
		e, aerr := s.acquire(r.fileIndex)
		if aerr != nil {
			return n, aerr
		}
		dst := b[n : n+int(r.length)]
		var rn int
		if e.m != nil {
			rn = copy(dst, e.m[r.offset:r.offset+r.length])
		} else {
			rn, err = e.f.ReadAt(dst, r.offset)
			if err == io.EOF && rn == int(r.length) {
				err = nil
			}
		}
		s.release(e)
		n += rn
		cur += int64(rn)
		if err != nil {
			return n, err
		}
	}
	if cur < end {
		n += zero(b[n : n+int(end-cur)])
	}
	if n < len(b) {
		err = io.EOF
	}
	return n, err
}

// zero clears b and returns its length.
func zero(b []byte) int {
	for i := range b {
		b[i] = 0
	}
	return len(b)
}

// WriteAt implements io.WriterAt over the span.
func (s *lazySpan) WriteAt(b []byte, off int64) (n int, err error) {
	s.closeMu.RLock()
	defer s.closeMu.RUnlock()
	if s.closed {
		return 0, ErrSpanClosed
	}
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	end := min(off+int64(len(b)), s.total)
	cur := off
	for _, r := range s.locate(off, int64(len(b))) {
		// Bytes aimed at a gap (pad region) have nowhere to go; they count
		// as written, as the pad file they stand for would hold zeros.
		if start := s.files[r.fileIndex].offset + r.offset; start > cur {
			n += int(start - cur)
			cur = start
		}
		e, aerr := s.acquire(r.fileIndex)
		if aerr != nil {
			return n, aerr
		}
		src := b[n : n+int(r.length)]
		var wn int
		if e.m != nil {
			wn = copy(e.m[r.offset:r.offset+r.length], src)
		} else {
			wn, err = e.f.WriteAt(src, r.offset)
		}
		s.release(e)
		n += wn
		cur += int64(wn)
		if err != nil {
			return n, err
		}
	}
	if cur < end {
		n += int(end - cur)
	}
	if n < len(b) {
		err = io.ErrShortWrite
	}
	return n, err
}

// withFile runs fn on file idx's descriptor and mapping (nil when the file is
// not mapped), opening the file if needed. Used for hole punching on
// eviction, which needs a descriptor whether or not the file is warm.
func (s *lazySpan) withFile(idx int, fn func(f *os.File, m mmap.MMap) error) error {
	s.closeMu.RLock()
	defer s.closeMu.RUnlock()
	if s.closed {
		return ErrSpanClosed
	}
	e, err := s.acquire(idx)
	if err != nil {
		return err
	}
	defer s.release(e)
	return fn(e.f, e.m)
}

// advise drops the page cache of [off, off+length) in file idx if the file
// is currently open — madvise on a mapping, fadvise on a descriptor. A cold
// file is not opened for advice: its pages will be reclaimed like any other.
// The two syscalls are hooked for tests.
var (
	madviseEvictFn = madviseEvict
	fadviseEvictFn = fadviseEvict
)

func adviseEvict(e *spanEntry, off, length int64) {
	if e.m != nil {
		end := min(off+length, int64(len(e.m)))
		if off < end {
			_ = madviseEvictFn(e.m[off:end])
		}
		return
	}
	_ = fadviseEvictFn(e.f, off, length)
}

func (s *lazySpan) advise(idx int, off, length int64) {
	s.closeMu.RLock()
	defer s.closeMu.RUnlock()
	if s.closed {
		return
	}
	s.mu.Lock()
	e, ok := s.open[idx]
	if !ok {
		s.mu.Unlock()
		return
	}
	e.refs++
	s.lru.MoveToFront(e.elem)
	s.mu.Unlock()
	adviseEvict(e, off, length)
	s.release(e)
}

// Flush syncs every open mapping to disk.
func (s *lazySpan) Flush() error {
	s.closeMu.RLock()
	defer s.closeMu.RUnlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	var err error
	for _, e := range s.open {
		if e.m != nil {
			err = errors.Join(err, e.m.Flush())
		}
	}
	return err
}

// evictPages advises the kernel to drop the pages of every open mapping.
func (s *lazySpan) evictPages() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.open {
		if e.m != nil {
			_ = madviseEvict(e.m)
		}
	}
}

// Close waits for in-flight reads and writes, then unmaps and closes every
// open file. Later reads and writes get ErrSpanClosed.
func (s *lazySpan) Close() error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var err error
	for idx, e := range s.open {
		err = errors.Join(err, s.closeEntryLocked(e))
		delete(s.open, idx)
	}
	s.lru.Init()
	return err
}

// mappedBytes is the sum of the lengths currently mapped (tests and metrics).
func (s *lazySpan) mappedBytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mapped
}

// openCount is the number of files currently open (tests and metrics).
func (s *lazySpan) openCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.open)
}

// acquire returns file idx open, with one reference taken, opening and
// mapping it on a miss and evicting idle files past maxOpen. The caller
// must release. Called with closeMu held for reading.
func (s *lazySpan) acquire(idx int) (*spanEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.open[idx]; ok {
		e.refs++
		s.lru.MoveToFront(e.elem)
		return e, nil
	}
	// Make room before opening, so bytes freed by an evicted mapping count
	// toward the new file's mapping decision.
	s.evictIdleLocked(1)
	e, err := s.openEntry(idx)
	if err != nil {
		return nil, err
	}
	e.refs = 1
	e.elem = s.lru.PushFront(e)
	s.open[idx] = e
	return e, nil
}

func (s *lazySpan) release(e *spanEntry) {
	s.mu.Lock()
	e.refs--
	s.mu.Unlock()
}

// evictIdleLocked closes least recently used files with no active reference
// until the table has room for `room` more within maxOpen. Files in use stay
// open even past the cap: a cap is a target, an unmap under a reader is a
// crash.
func (s *lazySpan) evictIdleLocked(room int) {
	for el := s.lru.Back(); el != nil && len(s.open)+room > s.maxOpen; {
		prev := el.Prev()
		e := el.Value.(*spanEntry)
		if e.refs == 0 {
			s.lru.Remove(el)
			delete(s.open, e.idx)
			_ = s.closeEntryLocked(e)
		}
		el = prev
	}
}

// openEntry opens (creating a sparse file of the right length if needed) and,
// for files within [mmapMin, mmapMax] while the mapped budget allows, maps
// file idx. Called with mu held.
func (s *lazySpan) openEntry(idx int) (*spanEntry, error) {
	sf := s.files[idx]
	if err := os.MkdirAll(filepath.Dir(sf.path), 0o750); err != nil {
		return nil, fmt.Errorf("making directory for %q: %w", sf.path, err)
	}
	f, err := os.OpenFile(sf.path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if fi.Size() < sf.length {
		if err := f.Truncate(sf.length); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	e := &spanEntry{idx: idx, f: f}
	if sf.length >= s.mmapMin && sf.length <= s.mmapMax && s.mapped+sf.length <= s.mmapBudget {
		n := int(sf.length)
		if int64(n) != sf.length {
			_ = f.Close()
			return nil, errors.New("file too large to map on this system")
		}
		m, err := mmap.MapRegion(f, n, mmap.RDWR, 0, 0)
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("mapping %q: %w", sf.path, err)
		}
		// Streaming access: aggressive readahead, pages reclaimed after use.
		_ = madviseSequential(m)
		e.m = m
		s.mapped += sf.length
	}
	return e, nil
}

// closeEntryLocked unmaps and closes e, returning its bytes to the mapped
// budget. Called with mu held.
func (s *lazySpan) closeEntryLocked(e *spanEntry) error {
	var err error
	if e.m != nil {
		s.mapped -= int64(len(e.m))
		err = e.m.Unmap()
		e.m = nil
	}
	if e.f != nil {
		err = errors.Join(err, e.f.Close())
		e.f = nil
	}
	return err
}
