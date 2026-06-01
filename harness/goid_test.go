package harness

import (
	"sync"
	"testing"
)

func TestGoidNonZeroAndStable(t *testing.T) {
	a := goid()
	b := goid()
	if a == 0 {
		t.Fatal("goid returned 0 on the test goroutine")
	}
	if a != b {
		t.Errorf("goid not stable within a goroutine: %d != %d", a, b)
	}
}

func TestGoidDistinctPerGoroutine(t *testing.T) {
	self := goid()
	var wg sync.WaitGroup
	var mu sync.Mutex
	ids := map[uint64]bool{}
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := goid()
			mu.Lock()
			ids[id] = true
			mu.Unlock()
		}()
	}
	wg.Wait()

	if ids[self] {
		t.Error("a child goroutine reported the parent's id")
	}
	if len(ids) != 16 {
		t.Errorf("expected 16 distinct goroutine ids, got %d", len(ids))
	}
}
