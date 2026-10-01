.PHONY: build test test-race vet fmt fmt-check check install system-test benchmark benchmark-apfs clean

build:
	mkdir -p bin
	go build -trimpath -o bin/cambium ./cmd/cambium
	go build -trimpath -o bin/git-cambium ./cmd/git-cambium

test:
	go test -count=1 ./...

test-race:
	go test -count=1 -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w $$(find cmd internal -name '*.go' -not -path './vendor/*')

fmt-check:
	@test -z "$$(gofmt -l $$(find cmd internal -name '*.go' -not -path './vendor/*'))" || \
	  (echo 'Go files need formatting:'; gofmt -l $$(find cmd internal -name '*.go' -not -path './vendor/*'); exit 1)

check: fmt-check vet test

install:
	go install ./cmd/cambium
	go install ./cmd/git-cambium

system-test: build
	CAMBIUM_BIN=$$(pwd)/bin/cambium scripts/system-test.sh

benchmark: build
	CAMBIUM_BIN=$$(pwd)/bin/cambium \
	  OUTPUT="$(OUTPUT)" METHODS="$(METHODS)" COUNT="$(COUNT)" \
	  FILES="$(FILES)" FILE_BYTES="$(FILE_BYTES)" \
	  ENV_FILES="$(ENV_FILES)" ENV_BYTES="$(ENV_BYTES)" \
	  scripts/benchmark.sh

benchmark-apfs: build
	@mkdir -p dist
	$(MAKE) benchmark OUTPUT=dist/benchmark-apfs.json METHODS=git,git-env-copy,cambium-auto,cambium-cow,simgit,cow COUNT=8 FILES=1000 FILE_BYTES=65536 ENV_FILES=500 ENV_BYTES=65536
	python3 scripts/assert-apfs-benchmark.py dist/benchmark-apfs.json

clean:
	rm -rf bin dist
