.PHONY: build test vet fmt fmt-check lint verify tidy fixture cover-floor hooks comment-drift

build:
	go build ./...

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

# fmt-check is the fast (<1s) pre-push gate mirroring CI's gofmt step exactly, so
# a formatting slip fails locally instead of costing a CI round-trip. `make verify`
# runs the same check, but only after build+vet+lint+test; this is the quick one.
fmt-check:
	@out=$$(gofmt -l . | grep -v '^testdata/fixtures/loansvc/' || true); \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	@echo "gofmt OK"

lint:
	golangci-lint run

# fixture builds and gates the hermetic fixture module (a separate module under
# testdata): its flows/ package is the behavioral snapshot gate, driven through
# the public harness/flow packages and run in go.work workspace mode.
fixture:
	cd testdata/fixtures/loansvc && go build ./... && go test ./...

# verify is the per-phase gate: it must stay green at the end of every phase.
verify: build vet lint test fixture
	@out=$$(gofmt -l . | grep -v '^testdata/fixtures/loansvc/' || true); \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
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
