package services

import (
	"container/list"
	"sync"
	"time"
)

const (
	// evictionProtectionWindow: a piece touched within it is what somebody is
	// reading right now, and it is evicted only after every older piece is
	// gone, protected or not. Recovered pieces get a lastAccess older than
	// this so they are immediately evictable at startup.
	evictionProtectionWindow = 30 * time.Second
)

type pieceEntry struct {
	index      int
	size       int64
	lastAccess time.Time
	element    *list.Element
}

// PieceLRU tracks piece access for a single torrent and computes eviction candidates
// when the per-torrent cache budget is exceeded.
type PieceLRU struct {
	mu          sync.Mutex
	entries     map[int]*pieceEntry  // pieceIndex → entry
	lruList     *list.List           // front = MRU, back = LRU
	used        int64                // current bytes used
	budget      int64                // max bytes (0 = unlimited)
	isProtected func(index int) bool // optional: returns true if piece should not be evicted (e.g. belongs to completed file)
}

// NewPieceLRU creates a new per-torrent LRU tracker.
// budget=0 means unlimited (no eviction).
func NewPieceLRU(budget int64) *PieceLRU {
	return &PieceLRU{
		entries: make(map[int]*pieceEntry),
		lruList: list.New(),
		budget:  budget,
	}
}

// SetProtectedFunc sets a callback that checks whether a piece should be
// protected from eviction (e.g. it belongs to a fully completed file that
// may be served directly from the filesystem cache).
func (l *PieceLRU) SetProtectedFunc(fn func(index int) bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.isProtected = fn
}

// Touch marks a piece as recently accessed, moving it to the front of the LRU list.
func (l *PieceLRU) Touch(index int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[index]
	if !ok {
		return
	}
	e.lastAccess = time.Now()
	l.lruList.MoveToFront(e.element)
}

// Add registers a completed piece in the LRU tracker and returns indices of pieces
// that should be evicted to stay within budget. The caller is responsible for
// actually evicting them (punch holes + mark incomplete).
func (l *PieceLRU) Add(index int, size int64) []int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.entries[index]; ok {
		// Already tracked — just touch it.
		e := l.entries[index]
		e.lastAccess = time.Now()
		l.lruList.MoveToFront(e.element)
		return nil
	}
	e := &pieceEntry{
		index:      index,
		size:       size,
		lastAccess: time.Now(),
	}
	e.element = l.lruList.PushFront(e)
	l.entries[index] = e
	l.used += size
	return l.computeEvictions()
}

// Remove removes a piece from the LRU tracker and decreases used bytes.
func (l *PieceLRU) Remove(index int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.removeLocked(index)
}

func (l *PieceLRU) removeLocked(index int) {
	e, ok := l.entries[index]
	if !ok {
		return
	}
	l.lruList.Remove(e.element)
	l.used -= e.size
	delete(l.entries, index)
}

// Used returns current disk usage in bytes.
func (l *PieceLRU) Used() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.used
}

// RemainingBudget returns how many bytes are still available before eviction starts.
func (l *PieceLRU) RemainingBudget() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.budget <= 0 {
		return 0
	}
	rem := l.budget - l.used
	if rem < 0 {
		return 0
	}
	return rem
}

// Recover restores LRU state from a map of pieceIndex → pieceSize (e.g. from SQLite).
// Recovered pieces get an old lastAccess time so they are immediately eligible for
// eviction if the cache exceeds the budget on startup.
func (l *PieceLRU) Recover(completePieces map[int]int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// Use a time older than evictionProtectionWindow so recovered pieces are evictable.
	oldTime := time.Now().Add(-evictionProtectionWindow - time.Minute)
	for index, size := range completePieces {
		if _, ok := l.entries[index]; ok {
			continue
		}
		e := &pieceEntry{
			index:      index,
			size:       size,
			lastAccess: oldTime,
		}
		e.element = l.lruList.PushBack(e) // recovered pieces go to back (LRU)
		l.entries[index] = e
		l.used += size
	}
}

// computeEvictions returns piece indices to evict (from LRU end) to bring used <= budget.
// Candidates, each group in LRU order, later groups only if the budget still is not met:
//  1. idle pieces NOT belonging to completed files;
//  2. idle completed-file pieces (file_completion is cleaned up by the caller);
//  3. recently accessed pieces not belonging to completed files;
//  4. recently accessed completed-file pieces.
//
// "Idle" is older than evictionProtectionWindow. The split is not cosmetic.
// Once the cache is full of pieces of completed files (a 400 GB series
// watched episode by episode), the one piece that is NOT protected is the
// piece that just finished for the stream being watched, and the old
// protected-last order evicted exactly it — on every completion. The
// reader then found a hole, re-requested the piece, it completed, was
// evicted again: 2,000 evictions and a 150 KB/s stream on one pod,
// 2026-09-22 (torrent 372e1b23). The stream's own piece has to outlive
// pieces nobody has touched for half an hour, protected or not.
//
// Must be called with l.mu held.
func (l *PieceLRU) computeEvictions() []int {
	if l.budget <= 0 || l.used <= l.budget {
		return nil
	}

	cutoff := time.Now().Add(-evictionProtectionWindow)
	var groups [4][]*pieceEntry
	for el := l.lruList.Back(); el != nil; el = el.Prev() {
		e := el.Value.(*pieceEntry)
		g := 0
		if e.lastAccess.After(cutoff) {
			g += 2
		}
		if l.isProtected != nil && l.isProtected(e.index) {
			g++
		}
		groups[g] = append(groups[g], e)
	}

	var toEvict []int
	simUsed := l.used
	for _, group := range groups {
		for _, e := range group {
			if simUsed <= l.budget {
				return toEvict
			}
			toEvict = append(toEvict, e.index)
			simUsed -= e.size
		}
	}
	return toEvict
}
