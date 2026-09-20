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
// 2026-09-20: half of the OOM kills were that, not the heap).
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
}

const (
	defaultMaxOpen = 2048
	defaultMmapMin = 64 * 1024
	defaultMmapMax = 4 << 30
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
	return c
}

// ErrSpanClosed is returned by reads and writes after Close.
var ErrSpanClosed = errors.New("storage span closed")

// newLazySpan lays out files (paths relative to nothing — they are given
// absolute) and opens none of them.
func newLazySpan(files []spanFile, cfg FileCacheConfig) *lazySpan {
	cfg = cfg.withDefaults()
	var off int64
	for i := range files {
		files[i].offset = off
		off += files[i].length
	}
	return &lazySpan{
		files:   files,
		total:   off,
		maxOpen: cfg.MaxOpen,
		mmapMin: cfg.MmapMin,
		mmapMax: cfg.MmapMax,
		open:    map[int]*spanEntry{},
		lru:     list.New(),
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
	for _, r := range s.locate(off, int64(len(b))) {
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
		if err != nil {
			return n, err
		}
	}
	if n < len(b) {
		err = io.EOF
	}
	return n, err
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
	for _, r := range s.locate(off, int64(len(b))) {
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
		if err != nil {
			return n, err
		}
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

// ifMapped runs fn on file idx's mapping only if the file is currently open
// and mapped. Page-cache advice on a cold file is not worth an open.
func (s *lazySpan) ifMapped(idx int, fn func(m mmap.MMap)) {
	s.closeMu.RLock()
	defer s.closeMu.RUnlock()
	if s.closed {
		return
	}
	s.mu.Lock()
	e, ok := s.open[idx]
	if !ok || e.m == nil {
		s.mu.Unlock()
		return
	}
	e.refs++
	s.lru.MoveToFront(e.elem)
	s.mu.Unlock()
	fn(e.m)
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
		err = errors.Join(err, closeEntry(e))
		delete(s.open, idx)
	}
	s.lru.Init()
	return err
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
	e, err := s.openEntry(idx)
	if err != nil {
		return nil, err
	}
	e.refs = 1
	e.elem = s.lru.PushFront(e)
	s.open[idx] = e
	s.evictIdleLocked()
	return e, nil
}

func (s *lazySpan) release(e *spanEntry) {
	s.mu.Lock()
	e.refs--
	s.mu.Unlock()
}

// evictIdleLocked closes least recently used files with no active reference
// until the table fits maxOpen. Files in use stay open even past the cap:
// a cap is a target, an unmap under a reader is a crash.
func (s *lazySpan) evictIdleLocked() {
	for el := s.lru.Back(); el != nil && len(s.open) > s.maxOpen; {
		prev := el.Prev()
		e := el.Value.(*spanEntry)
		if e.refs == 0 {
			s.lru.Remove(el)
			delete(s.open, e.idx)
			_ = closeEntry(e)
		}
		el = prev
	}
}

// openEntry opens (creating a sparse file of the right length if needed) and,
// for files within [mmapMin, mmapMax], maps file idx.
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
	if sf.length >= s.mmapMin && sf.length <= s.mmapMax {
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
	}
	return e, nil
}

func closeEntry(e *spanEntry) error {
	var err error
	if e.m != nil {
		err = e.m.Unmap()
		e.m = nil
	}
	if e.f != nil {
		err = errors.Join(err, e.f.Close())
		e.f = nil
	}
	return err
}
