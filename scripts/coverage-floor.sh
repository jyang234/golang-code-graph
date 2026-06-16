#!/usr/bin/env bash
# coverage-floor.sh asserts a non-regressing total statement-coverage floor for
# the engine module. The verdict engines are already well-covered; this guard
# exists so the *front-end* (loader/analyze/signatures) — whose bugs corrupt the
# graph silently rather than crash — cannot erode under refactoring while the
# engines stay green (see docs/design/ideas/front-end-test-hardening.md).
#
# It measures the engine module only ("./..."), matching the number the idea doc
# was written against. Override the floor with COVERAGE_FLOOR if the suite is
# deliberately raised:
#
#   COVERAGE_FLOOR=85 scripts/coverage-floor.sh
set -euo pipefail

FLOOR="${COVERAGE_FLOOR:-82}"
PROFILE="$(mktemp -t coverage.XXXXXX.out)"
trap 'rm -f "$PROFILE"' EXIT

# Per-package coverage merged into one profile. We do not use -coverpkg (which
# attributes cross-package coverage and needs the covdata tool); each package's
# own statements are what we floor.
go test -covermode=atomic -coverprofile="$PROFILE" ./... >/dev/null

TOTAL="$(go tool cover -func="$PROFILE" | awk '/^total:/ {gsub(/%/, "", $3); print $3}')"
if [ -z "$TOTAL" ]; then
	echo "coverage-floor: could not determine total coverage" >&2
	exit 1
fi

# Compare as floats without depending on bc.
below="$(awk -v t="$TOTAL" -v f="$FLOOR" 'BEGIN { print (t < f) ? 1 : 0 }')"
if [ "$below" -eq 1 ]; then
	echo "coverage-floor: FAIL — total coverage ${TOTAL}% is below the ${FLOOR}% floor" >&2
	echo "  The front-end (loader/analyze/signatures) is the likely eroder; add tests there." >&2
	exit 1
fi

echo "coverage-floor: OK — total coverage ${TOTAL}% >= ${FLOOR}% floor"
