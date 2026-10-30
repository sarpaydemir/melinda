GO        ?= go
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE      ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS   := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

TEST_DB_IMAGE ?= pgvector/pgvector:pg17
TEST_DB_NAME  ?= dost-test-db
TEST_DB_PORT  ?= 55432
TEST_DB_URL   := postgres://postgres:postgres@localhost:$(TEST_DB_PORT)/postgres?sslmode=disable

.DEFAULT_GOAL := help
.PHONY: help check fmt fmt-check vet staticcheck test test-integration vuln build tidy clean

help: ## List targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-18s %s\n", $$1, $$2}'

check: fmt-check vet staticcheck test ## All local quality gates; must pass before every PR

fmt: ## Format all Go code
	gofmt -w $$($(GO) list -f '{{.Dir}}' ./...)

fmt-check: ## Fail if any Go file needs gofmt
	@out=$$(gofmt -l $$($(GO) list -f '{{.Dir}}' ./...)); \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet: ## go vet
	$(GO) vet ./...

staticcheck: ## staticcheck (pinned in go.mod)
	$(GO) tool staticcheck ./...

test: ## Unit tests with the race detector
	$(GO) test -race -count=1 ./...

test-integration: ## Integration tests against a throwaway pgvector container (needs Docker)
	@docker rm -f $(TEST_DB_NAME) >/dev/null 2>&1 || true
	@docker run -d --rm --name $(TEST_DB_NAME) -e POSTGRES_PASSWORD=postgres \
		-p $(TEST_DB_PORT):5432 $(TEST_DB_IMAGE) >/dev/null
	@trap 'docker rm -f $(TEST_DB_NAME) >/dev/null 2>&1' EXIT; \
	for i in $$(seq 1 30); do docker exec $(TEST_DB_NAME) pg_isready -U postgres >/dev/null 2>&1 && break; sleep 1; done; \
	DOST_TEST_DATABASE_URL='$(TEST_DB_URL)' $(GO) test -race -count=1 -tags=integration ./...

vuln: ## Known-vulnerability scan (pinned in go.mod)
	$(GO) tool govulncheck ./...

build: ## Static binary in bin/
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/dost ./cmd/dost

tidy: ## go mod tidy
	$(GO) mod tidy

clean: ## Remove build output
	rm -rf bin dist
