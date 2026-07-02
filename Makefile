.PHONY: build test vet fmt fmt-check lint verify tidy fixture cover-floor hooks comment-drift

# Pinned to match .github/workflows/gates.yml exactly. CLAUDE.md's trust boundary
# ("CI mirrors make verify exactly") requires the same linter build both places;
# `lint` warns loudly when the locally-installed version drifts from this pin.
GOLANGCI_LINT_VERSION ?= v2.5.0

# Repo-root gofmt exclusion, defined ONCE here and referenced only by `fmt-check`
# (which `verify` now calls instead of re-inlining the check) so the predicate
# cannot drift into two copies — CLAUDE.md one-source-of-truth (R-6). Only loansvc
# is excluded: it is the pre-existing carve-out. impeachsvc is deliberately NOT
# excluded so it stays gofmt-gated here (the `fixture` target only builds+tests, so
# excluding it would leave its formatting ungated anywhere — a coverage loss).
GOFMT_EXCLUDE = ^testdata/fixtures/loansvc/

build:
	go build ./...

# -race mirrors CI's `go test -race` exactly (gates.yml): a data race that would
# fail CI must fail `make test`/`make verify` locally first, not cost a round-trip.
# Trust parity outranks the slower run here (prime directive: speed loses).
test:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

# fmt-check is the fast (<1s) pre-push gate mirroring CI's gofmt step exactly, so
# a formatting slip fails locally instead of costing a CI round-trip. `make verify`
# runs the same check, but only after build+vet+lint+test; this is the quick one.
fmt-check:
	@out=$$(gofmt -l . | grep -vE '$(GOFMT_EXCLUDE)' || true); \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	@echo "gofmt OK"

lint:
	@have=$$(golangci-lint version 2>/dev/null | grep -oE 'version v?[0-9]+\.[0-9]+\.[0-9]+' | grep -oE 'v?[0-9]+\.[0-9]+\.[0-9]+' | head -1); \
	if [ -n "$$have" ] && [ "v$${have#v}" != "$(GOLANGCI_LINT_VERSION)" ]; then \
		echo "warning: golangci-lint $$have differs from CI pin $(GOLANGCI_LINT_VERSION); results may diverge from CI" >&2; \
	fi
	golangci-lint run

# fixture builds and gates the hermetic fixture modules (separate modules under
# testdata): their flows/ packages are the behavioral snapshot gates, driven
# through the public harness/flow packages and run in go.work workspace mode.
# impeachsvc is the behavioral-impeachment fixture (a missed-route DB DELETE), so
# its captured trace golden is gated here alongside loansvc's.
fixture:
	cd testdata/fixtures/loansvc && go build ./... && go test -race ./...
	cd testdata/fixtures/impeachsvc && go build ./... && go test -race ./...

# verify is the per-phase gate: it must stay green at the end of every phase. It
# reuses the fmt-check target (rather than re-inlining the gofmt gate) so the
# exclusion predicate has exactly one home (R-6).
verify: build vet lint test fixture fmt-check
	@echo "verify OK"

tidy:
	go mod tidy

# cover-floor asserts the non-regressing total-coverage floor (front-end-test-
# hardening idea, Item 1). Override with COVERAGE_FLOOR to raise the bar.
cover-floor:
	scripts/coverage-floor.sh

# hooks points git at the repo's tracked hooks (the comment-drift nudge). One-time
# per clone; safe to re-run.
hooks:
	git config core.hooksPath scripts/hooks
	@echo "git hooks installed (core.hooksPath=scripts/hooks)"

# comment-drift runs the advisory nudge against the staged diff on demand. Set
# COMMENTDRIFT_STRICT=1 to make it exit non-zero on a finding.
comment-drift:
	@go run ./scripts/commentdrift
