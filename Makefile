BINARY  := riverbed
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
GOFLAGS := -trimpath
TAGS    := osusergo,netgo
IMAGE   ?= riverbed

PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

export CGO_ENABLED = 0

.PHONY: all help build test vet fmt check dist clean docker docker-run run

all: check build ## Vet, test and build.

help: ## Show help for each of the Makefile recipes.
	@grep -E '^[a-zA-Z0-9_-]+:[^#]*## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":[^#]*## "} \
		{names[NR] = $$1; descs[NR] = $$2; if (length($$1) > width) width = length($$1)} \
		END {for (i = 1; i <= NR; i++) printf "\033[36m%-" width "s\033[0m    %s\n", names[i], descs[i]}'

build: ## Build ./riverbed for this platform.
	go build $(GOFLAGS) -tags $(TAGS) -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/riverbed

test: ## Run the tests.
	go test ./...

vet: ## Run go vet.
	go vet ./...

fmt: ## Format every Go file in place.
	gofmt -l -w .

check: vet test ## Vet, test, and fail if anything is unformatted.
	@test -z "$$(gofmt -l . | tee /dev/stderr)" || { echo "run make fmt"; exit 1; }

dist: clean ## Build a static binary for every supported platform into dist/.
	@mkdir -p dist
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		echo "building dist/$(BINARY)-$$os-$$arch"; \
		GOOS=$$os GOARCH=$$arch go build $(GOFLAGS) -tags $(TAGS) \
			-ldflags "$(LDFLAGS)" -o dist/$(BINARY)-$$os-$$arch ./cmd/riverbed || exit 1; \
	done
	@cd dist && shasum -a 256 * > SHA256SUMS && cat SHA256SUMS

docker: ## Build the container image.
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

docker-run: ## Run the container image against ./riverbed.toml.
	docker run --rm -p 8080:8080 \
		-v riverbed-data:/var/lib/riverbed \
		-v $(PWD)/riverbed.toml:/etc/riverbed/riverbed.toml:ro \
		-e RIVERBED_WEBHOOK_TOKEN \
		$(IMAGE):latest

run: build ## Build, then serve with ./riverbed.toml.
	./$(BINARY) serve -config riverbed.toml

clean: ## Remove the binary and dist/.
	rm -rf dist $(BINARY)
