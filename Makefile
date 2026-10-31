GO        ?= go
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE      ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS   := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

TEST_DB_IMAGE ?= pgvector/pgvector:pg17

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
	@set -e; name="dost-test-$$$$-$$(date +%s)"; \
	trap 'docker rm -f "$$name" >/dev/null 2>&1 || true' EXIT; \
	trap 'exit 130' INT TERM HUP QUIT; \
	docker run -d --rm --name "$$name" -e POSTGRES_PASSWORD=postgres \
		-p 127.0.0.1::5432 $(TEST_DB_IMAGE) >/dev/null; \
	port=$$(docker port "$$name" 5432/tcp | head -n1 | sed 's/.*://'); \
	[ -n "$$port" ] || { echo "could not determine test database port" >&2; exit 1; }; \
	ready=; for i in $$(seq 1 60); do \
		docker exec "$$name" pg_isready -h 127.0.0.1 -U postgres >/dev/null 2>&1 && ready=1 && break; sleep 1; done; \
	[ -n "$$ready" ] || { echo "test database did not become ready" >&2; exit 1; }; \
	DOST_TEST_DATABASE_URL="postgres://postgres:postgres@127.0.0.1:$$port/postgres?sslmode=disable" \
		$(GO) test -race -count=1 -tags=integration ./...

vuln: ## Known-vulnerability scan (pinned in go.mod)
	$(GO) tool govulncheck ./...

build: ## Static binary in bin/
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/dost ./cmd/dost

tidy: ## go mod tidy
	$(GO) mod tidy

clean: ## Remove build output
	rm -rf bin dist
