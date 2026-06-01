package harness

import (
	"bytes"
	"runtime"
	"strconv"
)

// goid returns the current goroutine's id, flowmap's structural concurrency
// signal (canon §3.3 / plan [C2]): two sibling spans that started on goroutines
// other than their parent's were dispatched concurrently, regardless of how
// their wall-clock intervals happened to fall.
//
// Go exposes no public goroutine id, so this parses the runtime stack header
// ("goroutine N [state]:"). That is a deliberate, contained hack confined to the
// test-harness boundary. It is also best-effort: on any failure it returns 0,
// and capture.Concurrent then falls back to caller-clock interval overlap. A
// regression in this parsing therefore degrades to the documented fallback and
// surfaces as a determinism self-test failure — never a silently wrong golden.
func goid() uint64 {
	// The first stack line is short ("goroutine 18446744073709551615 [running]:"
	// is ~40 bytes), but use a comfortable buffer so a future format change can't
	// truncate the id mid-digits.
	var buf [128]byte
	n := runtime.Stack(buf[:], false)
	fields := bytes.Fields(buf[:n])
	if len(fields) < 2 {
		return 0
	}
	id, err := strconv.ParseUint(string(fields[1]), 10, 64)
	if err != nil {
		return 0
	}
	return id
}
