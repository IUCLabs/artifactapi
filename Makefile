.PHONY: build test lint fmt e2e docker-e2e docker docker-ui compose clean tidy check-go

BINARY := bin/artifactapi
MODULE := github.com/IUCLabs/artifactapi
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "0.0.0-dev")
GO_VERSION_REQUIRED := 1.23
GO_VERSION_ACTUAL := $(shell go version | sed 's/go version go\([0-9]*\.[0-9]*\).*/\1/')

# git.example.com modules (golib) are fetched straight from Gitea, never via the
# public proxy or sum db, which have no view of them. Exported here rather than
# written with `go env -w`, so a fresh checkout needs no machine-local setup.
export GOPRIVATE := git.example.com

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
	TESTCONTAINERS_RYUK_DISABLED=true go test -tags=e2e -race -count=1 -timeout=5m ./e2e/...

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

# Bump helpers — reads the latest semver tag and creates the next one.
_LATEST := $(shell git tag --sort=-v:refname | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$$' | head -1)
_BASE   := $(if $(_LATEST),$(_LATEST),v0.0.0)
_MAJ    := $(shell echo $(_BASE) | sed 's/^v//' | cut -d. -f1)
_MIN    := $(shell echo $(_BASE) | sed 's/^v//' | cut -d. -f2)
_PAT    := $(shell echo $(_BASE) | sed 's/^v//' | cut -d. -f3)

patch:
	@NEW=v$(_MAJ).$(_MIN).$(shell expr $(_PAT) + 1); \
	git tag $$NEW && echo "Tagged $$NEW" && $(MAKE) _tag TAG=$$NEW

minor:
	@NEW=v$(_MAJ).$(shell expr $(_MIN) + 1).0; \
	git tag $$NEW && echo "Tagged $$NEW" && $(MAKE) _tag TAG=$$NEW

major:
	@NEW=v$(shell expr $(_MAJ) + 1).0.0; \
	git tag $$NEW && echo "Tagged $$NEW" && $(MAKE) _tag TAG=$$NEW

_tag:
	git push origin $(TAG)
