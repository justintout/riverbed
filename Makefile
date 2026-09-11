# Riverbed is pure Go, so every build is static and cross compiles without a
# toolchain for the target.
BINARY  := riverbed
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
GOFLAGS := -trimpath
TAGS    := osusergo,netgo
IMAGE   ?= riverbed

# Because nothing links libc, one binary per architecture covers Debian,
# Ubuntu, Fedora, Alpine and the rest.
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

export CGO_ENABLED = 0

.PHONY: all build test vet fmt check dist clean docker docker-run run
all: check build

build:
	go build $(GOFLAGS) -tags $(TAGS) -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/riverbed

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

check: vet test
	@test -z "$$(gofmt -l . | tee /dev/stderr)" || { echo "run make fmt"; exit 1; }

# dist writes one static binary per platform into dist/.
dist: clean
	@mkdir -p dist
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		echo "building dist/$(BINARY)-$$os-$$arch"; \
		GOOS=$$os GOARCH=$$arch go build $(GOFLAGS) -tags $(TAGS) \
			-ldflags "$(LDFLAGS)" -o dist/$(BINARY)-$$os-$$arch ./cmd/riverbed || exit 1; \
	done
	@cd dist && shasum -a 256 * > SHA256SUMS && cat SHA256SUMS

docker:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

docker-run:
	docker run --rm -p 8080:8080 \
		-v riverbed-data:/var/lib/riverbed \
		-v $(PWD)/riverbed.toml:/etc/riverbed/riverbed.toml:ro \
		-e RIVERBED_WEBHOOK_TOKEN \
		$(IMAGE):latest

run: build
	./$(BINARY) serve -config riverbed.toml

clean:
	rm -rf dist $(BINARY)
