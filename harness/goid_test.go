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

// recordTB is a TB whose Fatalf records rather than aborts, so the fail-closed
// branch of the goid self-check is observable in a test.
type recordTB struct{ fatal string }

func (r *recordTB) Helper()                           {}
func (r *recordTB) Fatalf(format string, args ...any) { r.fatal = format }

// TestGoidSelfCheckFiresOnParseFailure is the M-1 regression: when the probe
// reports 0 (a stack-header parse regression), harness construction must fail
// loudly through TB rather than silently degrade the structural concurrency
// signal. The healthy case must stay quiet.
func TestGoidSelfCheckFiresOnParseFailure(t *testing.T) {
	var broken recordTB
	failUnlessGoidOK(&broken, false)
	if broken.fatal == "" {
		t.Fatal("goid self-check did not fail when the probe returned 0")
	}

	var healthy recordTB
	failUnlessGoidOK(&healthy, true)
	if healthy.fatal != "" {
		t.Fatalf("goid self-check fired on a working probe: %q", healthy.fatal)
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
