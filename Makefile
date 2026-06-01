.PHONY: build test vet fmt lint verify tidy fixture

build:
	go build ./...

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

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
