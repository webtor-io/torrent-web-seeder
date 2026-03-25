package services

import (
	"testing"
	"time"
)

func TestNewPieceLRU_Unlimited(t *testing.T) {
	lru := NewPieceLRU(0)
	if lru.budget != 0 {
		t.Fatalf("expected budget 0, got %d", lru.budget)
	}
	toEvict := lru.Add(0, 1000)
	if len(toEvict) != 0 {
		t.Fatalf("expected no eviction with unlimited budget, got %v", toEvict)
	}
	if lru.Used() != 1000 {
		t.Fatalf("expected used 1000, got %d", lru.Used())
	}
}

func TestPieceLRU_AddAndEvict(t *testing.T) {
	lru := NewPieceLRU(100)

	lru.Add(0, 30)
	lru.Add(1, 30)
	lru.Add(2, 30)
	if lru.Used() != 90 {
		t.Fatalf("expected used 90, got %d", lru.Used())
	}

	// Adding piece 3 pushes over budget (120 > 100). Piece 0 is LRU → evicted.
	toEvict := lru.Add(3, 30)
	if len(toEvict) == 0 {
		t.Fatal("expected eviction candidates")
	}

	for _, idx := range toEvict {
		lru.Remove(idx)
	}
	if lru.Used() > 100 {
		t.Fatalf("expected used <= 100 after eviction, got %d", lru.Used())
	}
}

func TestPieceLRU_TouchPromotesToFront(t *testing.T) {
	lru := NewPieceLRU(100)

	lru.Add(0, 40) // back (LRU)
	lru.Add(1, 40)
	lru.Add(2, 40) // front (MRU)

	// Touch piece 0 — moves to front. Now LRU order: 1, 2, 0.
	lru.Touch(0)

	// Add piece 3 → total=160 > 100. Eviction from back → piece 1 first.
	toEvict := lru.Add(3, 40)
	if len(toEvict) == 0 {
		t.Fatal("expected eviction")
	}
	for _, idx := range toEvict {
		if idx == 0 {
			t.Fatal("piece 0 was recently touched, should not be evicted")
		}
	}
}

func TestPieceLRU_ImmediateEviction(t *testing.T) {
	// With no time-based protection, eviction happens immediately.
	lru := NewPieceLRU(50)

	lru.Add(0, 30)
	lru.Add(1, 30) // total=60 > 50, piece 0 evicted immediately
	toEvict := lru.Add(1, 30)
	// Already tracked piece 1, no new eviction from re-add.
	_ = toEvict

	// The first Add(1,30) should have returned eviction for piece 0.
	// Let's test directly:
	lru2 := NewPieceLRU(50)
	lru2.Add(0, 30)
	evict := lru2.Add(1, 30) // total=60 > 50
	if len(evict) == 0 {
		t.Fatal("expected immediate eviction")
	}
	if evict[0] != 0 {
		t.Fatalf("expected piece 0 evicted, got %d", evict[0])
	}
}

func TestPieceLRU_Remove(t *testing.T) {
	lru := NewPieceLRU(0)
	lru.Add(0, 100)
	lru.Add(1, 200)
	if lru.Used() != 300 {
		t.Fatalf("expected 300, got %d", lru.Used())
	}
	lru.Remove(0)
	if lru.Used() != 200 {
		t.Fatalf("expected 200 after remove, got %d", lru.Used())
	}
	lru.Remove(99) // non-existent — no panic
	if lru.Used() != 200 {
		t.Fatalf("expected 200 after noop remove, got %d", lru.Used())
	}
}

func TestPieceLRU_AddDuplicate(t *testing.T) {
	lru := NewPieceLRU(0)
	lru.Add(0, 100)
	lru.Add(0, 100) // duplicate — just touch, not double-count
	if lru.Used() != 100 {
		t.Fatalf("expected 100 (no double-count), got %d", lru.Used())
	}
}

func TestPieceLRU_Recover(t *testing.T) {
	lru := NewPieceLRU(500)
	completePieces := map[int]int64{
		0:  100,
		5:  200,
		10: 300,
	}
	lru.Recover(completePieces)
	if lru.Used() != 600 {
		t.Fatalf("expected 600 after recovery, got %d", lru.Used())
	}
	lru.mu.Lock()
	if len(lru.entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(lru.entries))
	}
	lru.mu.Unlock()

	// Recovered pieces have old lastAccess and are at back of LRU.
	// Adding a piece should evict recovered ones.
	toEvict := lru.Add(20, 100) // total=700 > 500
	if len(toEvict) == 0 {
		t.Fatal("expected eviction after adding over budget")
	}
}

func TestPieceLRU_RecoverOldTimestamp(t *testing.T) {
	lru := NewPieceLRU(100)
	lru.Recover(map[int]int64{0: 50, 1: 50, 2: 50}) // total=150 > 100

	// Recovered pieces should have old lastAccess, so evictOverBudget can work immediately.
	lru.mu.Lock()
	for _, e := range lru.entries {
		if time.Since(e.lastAccess) < time.Minute {
			t.Fatal("recovered pieces should have old lastAccess")
		}
	}
	lru.mu.Unlock()
}

func TestPieceLRU_RemainingBudget(t *testing.T) {
	lru := NewPieceLRU(100)
	if lru.RemainingBudget() != 100 {
		t.Fatalf("expected 100, got %d", lru.RemainingBudget())
	}
	lru.Add(0, 60)
	if lru.RemainingBudget() != 40 {
		t.Fatalf("expected 40, got %d", lru.RemainingBudget())
	}
}

func TestPieceLRU_RemainingBudget_Unlimited(t *testing.T) {
	lru := NewPieceLRU(0)
	lru.Add(0, 9999)
	if lru.RemainingBudget() != 0 {
		t.Fatalf("expected 0 for unlimited, got %d", lru.RemainingBudget())
	}
}

func TestPieceLRU_EvictionOrder(t *testing.T) {
	lru := NewPieceLRU(100)

	// Add in order: 0 is oldest (back of LRU), 3 is newest (front).
	lru.Add(0, 30) // back
	lru.Add(1, 30)
	lru.Add(2, 30)
	// total=90 < 100, no eviction yet

	// Add piece 3 → total=120 > 100, need to evict 20+ bytes.
	// Piece 0 is LRU → evicted first.
	toEvict := lru.Add(3, 30)
	if len(toEvict) < 1 {
		t.Fatal("expected at least 1 eviction")
	}
	if toEvict[0] != 0 {
		t.Fatalf("expected piece 0 to be evicted first, got %d", toEvict[0])
	}
}

func TestPieceLRU_TouchNonExistent(t *testing.T) {
	lru := NewPieceLRU(100)
	lru.Touch(42) // should not panic
}

func TestPieceLRU_ProtectedPiecesEvictedLast(t *testing.T) {
	lru := NewPieceLRU(100)

	// Pieces 0,1 are "protected" (belong to completed file).
	lru.SetProtectedFunc(func(index int) bool {
		return index == 0 || index == 1
	})

	lru.Add(0, 30) // protected, back
	lru.Add(1, 30) // protected
	lru.Add(2, 30) // unprotected
	lru.Add(3, 30) // unprotected, front
	// total=120 > 100 → eviction triggered on Add(3)
	// Pieces 0,1 are protected → skip in pass 1
	// Pieces 2 is unprotected LRU → evicted first

	// Let's test from scratch to verify ordering clearly.
	lru2 := NewPieceLRU(70)
	lru2.SetProtectedFunc(func(index int) bool {
		return index == 0 || index == 1
	})
	lru2.Add(0, 30) // protected
	lru2.Add(1, 30) // protected
	lru2.Add(2, 30) // unprotected → total=90 > 70

	toEvict := lru2.Add(2, 30) // already exists, just touch
	_ = toEvict
	// Trigger via new piece:
	lru3 := NewPieceLRU(70)
	lru3.SetProtectedFunc(func(index int) bool {
		return index == 0
	})
	lru3.Add(0, 30) // protected
	lru3.Add(1, 30) // unprotected
	// total=60 < 70
	toEvict = lru3.Add(2, 30) // total=90 > 70, evict piece 1 (unprotected LRU)
	if len(toEvict) == 0 {
		t.Fatal("expected eviction")
	}
	if toEvict[0] != 1 {
		t.Fatalf("expected unprotected piece 1 evicted, got %d", toEvict[0])
	}
}

func TestPieceLRU_ProtectedPiecesNotEvictedWhenNotNeeded(t *testing.T) {
	lru := NewPieceLRU(90)
	lru.SetProtectedFunc(func(index int) bool {
		return index == 0
	})

	lru.Add(0, 30) // protected
	lru.Add(1, 30) // unprotected
	// total=60 < 90
	toEvict := lru.Add(2, 30) // total=90 == 90, no eviction needed
	if len(toEvict) != 0 {
		t.Fatalf("expected no eviction at exactly budget, got %v", toEvict)
	}

	toEvict = lru.Add(3, 30) // total=120 > 90, evict unprotected first
	for _, idx := range toEvict {
		if idx == 0 {
			t.Fatal("protected piece 0 should not be evicted when unprotected pieces suffice")
		}
	}
}
