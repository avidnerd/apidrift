# apidrift build tooling.
#
# Three functions carry the system's judgement -- schema.Merge, detect.Detect
# and eval.Score -- and each was specified before it was written. Their 66
# contract tests can be run on their own, so work on the core is isolated from
# the rest of the suite:
#
#   make test             every test
#   make test-impl        all but the three contract suites
#   make test-contracts   only the three contract suites
#
# The convention this rests on: a test that specifies one of those functions is
# named Test<Function>..., and nothing else is.

GO ?= go
PKGS := ./...
CONTRACT_TESTS := ^(TestMerge|TestDetect|TestScore)
BIN := bin/apidrift

.PHONY: all build test test-impl test-contracts race lint fmt vet bench clean

all: lint test-impl build

build:
	$(GO) build -o $(BIN) ./cmd/apidrift

## test runs the whole suite under the race detector. This is what CI gates on.
test:
	$(GO) test -race $(PKGS)

## test-impl runs every test except the three contract suites.
test-impl:
	$(GO) test -race -skip '$(CONTRACT_TESTS)' $(PKGS)

## test-contracts runs only the three contract suites.
test-contracts:
	$(GO) test -run '$(CONTRACT_TESTS)' $(PKGS)

race: test-impl

lint: fmt vet

## fmt reports rather than rewriting, so CI and local behave identically.
fmt:
	@out=$$(gofmt -l .); \
	if [ -n "$$out" ]; then echo "not gofmt'd:"; echo "$$out"; exit 1; fi

vet:
	$(GO) vet $(PKGS)

bench:
	$(GO) test -run '^$$' -bench . -benchmem $(PKGS)

clean:
	rm -rf bin
	$(GO) clean -testcache
