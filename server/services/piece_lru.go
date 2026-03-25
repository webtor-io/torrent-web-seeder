package services

import (
	"container/list"
	"sync"
	"time"
)

const (
	// evictionProtectionWindow is used to set old lastAccess on recovered pieces
	// so they are immediately evictable at startup.
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
	entries     map[int]*pieceEntry // pieceIndex → entry
	lruList     *list.List          // front = MRU, back = LRU
	used        int64               // current bytes used
	budget      int64               // max bytes (0 = unlimited)
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
// Two-pass strategy:
//   - Pass 1: evict pieces NOT belonging to completed files (safe, no race with file cache).
//   - Pass 2: if still over budget, evict completed-file pieces (file_completion will be cleaned up by caller).
//
// LRU ordering naturally protects actively-read pieces — they are at the front
// (recently Touched), eviction happens from the back.
//
// Must be called with l.mu held.
func (l *PieceLRU) computeEvictions() []int {
	if l.budget <= 0 || l.used <= l.budget {
		return nil
	}

	var toEvict []int
	var protectedCandidates []*pieceEntry
	simUsed := l.used

	// Pass 1: evict non-protected pieces first (from LRU end).
	for el := l.lruList.Back(); el != nil && simUsed > l.budget; el = el.Prev() {
		e := el.Value.(*pieceEntry)
		if l.isProtected != nil && l.isProtected(e.index) {
			protectedCandidates = append(protectedCandidates, e)
			continue
		}
		toEvict = append(toEvict, e.index)
		simUsed -= e.size
	}

	// Pass 2: if still over budget, evict protected (completed-file) pieces.
	for _, e := range protectedCandidates {
		if simUsed <= l.budget {
			break
		}
		toEvict = append(toEvict, e.index)
		simUsed -= e.size
	}

	return toEvict
}
