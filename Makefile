.PHONY: build test lint fmt e2e docker-e2e docker docker-ui compose clean tidy check-go patch minor major

BINARY := bin/artifactapi
MODULE := github.com/IUCLabs/artifactapi
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "0.0.0-dev")
GO_VERSION_REQUIRED := 1.23
GO_VERSION_ACTUAL := $(shell go version | sed 's/go version go\([0-9]*\.[0-9]*\).*/\1/')

check-go:
	@if [ "$$(printf '%s\n%s' "$(GO_VERSION_REQUIRED)" "$(GO_VERSION_ACTUAL)" | sort -V | head -1)" != "$(GO_VERSION_REQUIRED)" ]; then \
		echo "ERROR: Go >= $(GO_VERSION_REQUIRED) required, found $(GO_VERSION_ACTUAL)"; exit 1; \
	fi

build: check-go tidy
	go build -ldflags="-s -w -X main.version=$(VERSION)" -o $(BINARY) ./cmd/artifactapi

test: check-go
	go test -race -count=1 ./pkg/... ./internal/...

lint: check-go
	golangci-lint run ./...
	go vet ./...

fmt: check-go
	gofmt -w .
	goimports -w .

e2e: check-go
	TESTCONTAINERS_RYUK_DISABLED=true go test -tags=e2e -count=1 -timeout=15m -v ./e2e/...

# Build the container, bring up the full docker-compose stack + a mock upstream,
# and run the black-box suite against the running product.
docker-e2e: check-go
	./scripts/docker-e2e.sh

docker:
	docker build -t artifactapi:$(VERSION) .

docker-ui:
	docker build -t artifactapi-ui:$(VERSION) -f ui/Dockerfile.ui ui/

compose:
	docker compose up -d

clean:
	rm -rf bin/

tidy:
	go mod tidy

DEFAULT_BRANCH ?= main

# Publish the release with generated notes, then push the tag; no workflow
# fills the release, so it is not drafted.
patch minor major:
	@git fetch --quiet --tags origin $(DEFAULT_BRANCH)
	@test "$$(git branch --show-current)" = "$(DEFAULT_BRANCH)" || { echo "ERROR: not on $(DEFAULT_BRANCH)"; exit 1; }
	@test -z "$$(git status --porcelain)" || { echo "ERROR: working tree not clean"; exit 1; }
	@test "$$(git rev-parse HEAD)" = "$$(git rev-parse origin/$(DEFAULT_BRANCH))" || { echo "ERROR: HEAD is not origin/$(DEFAULT_BRANCH)"; exit 1; }
	@LATEST=$$(git tag --list 'v*' --sort=-v:refname | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$$' | head -1); \
	NEW=$$(echo "$${LATEST:-v0.0.0}" | sed 's/^v//' | awk -F. -v b=$@ '{ \
		if (b == "major") print "v" ($$1+1) ".0.0"; \
		else if (b == "minor") print "v" $$1 "." ($$2+1) ".0"; \
		else print "v" $$1 "." $$2 "." ($$3+1) }'); \
	SHA=$$(git rev-parse HEAD); \
	gh release create "$$NEW" --target "$$SHA" --generate-notes --title "$$NEW" || exit 1; \
	git push origin "$$SHA:refs/tags/$$NEW" || { gh release delete "$$NEW" --yes; exit 1; }
