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
// test-harness boundary. It is best-effort: on any failure it returns 0, and
// capture.Concurrent then falls back to caller-clock interval overlap. A silent
// fallback would degrade every span's structural concurrency signal, so
// NewInProcess probes goid() once and fails loudly through TB when it returns 0
// (see the goidCheck in harness.go) — inside this repo that surfaces as a test
// failure, and in a consumer repo it surfaces at harness construction rather than
// as a subtly-wrong golden.
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
