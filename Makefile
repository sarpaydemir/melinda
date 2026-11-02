GO        ?= go
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE      ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS   := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

TEST_DB_IMAGE ?= pgvector/pgvector:pg17
DURATION      ?= 3m

.DEFAULT_GOAL := help
.PHONY: help check fmt fmt-check vet staticcheck test test-integration test-e2e test-load test-soak vuln build image release release-dry-run tidy clean

help: ## List targets
	@LC_ALL=C grep -E '^[a-z0-9-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-18s %s\n", $$1, $$2}'

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

# with-test-db runs $(1) with DOST_TEST_DATABASE_URL naming a throwaway pgvector container.
define with-test-db
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
	DOST_TEST_DATABASE_URL="postgres://postgres:postgres@127.0.0.1:$$port/postgres?sslmode=disable" $(1)
endef

test-integration: ## Integration tests against a throwaway pgvector container (needs Docker)
	$(call with-test-db,$(GO) test -race -count=1 -tags=integration ./...)

test-e2e: ## End-to-end scenarios: the race-enabled binary against fakes and a throwaway pgvector container (needs Docker)
	@mkdir -p bin && $(GO) build -race -o bin/melinda-e2e ./cmd/melinda
	$(call with-test-db,DOST_E2E_BINARY=$(CURDIR)/bin/melinda-e2e $(GO) test -count=1 -timeout=5m -tags=e2e $(E2E_FLAGS) ./test/e2e/)

test-load: ## Memory load test: worst-case concurrent replies under the default container memory limit
	$(GO) test -count=1 -tags=load -run ContainerLimit -v ./internal/app/

test-soak: ## Soak test (needs Docker): make test-soak DURATION=2h OUT=<dir for log and profiles>
	$(call with-test-db,DOST_SOAK_DURATION=$(DURATION) DOST_SOAK_OUT=$(OUT) $(GO) test -count=1 -tags=soak -run Soak -timeout 0 -v ./internal/app/)

vuln: ## Known-vulnerability scan (pinned in go.mod)
	$(GO) tool govulncheck ./...

build: ## Static binary in bin/
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/melinda ./cmd/melinda

image: ## Container image dost:$(VERSION), also tagged dost:latest
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) -t dost:$(VERSION) -t dost:latest .

release: ## Run gates, then tag and publish binaries and image: make release VERSION=vX.Y.Z
	@test "$(origin VERSION)" = "command line" || { echo "usage: make release VERSION=vX.Y.Z" >&2; exit 1; }
	scripts/release.sh preflight $(VERSION)
	$(MAKE) check test-integration vuln
	scripts/release.sh publish $(VERSION)

release-dry-run: ## Build every release artifact into dist/ without tagging, pushing, or publishing
	scripts/release.sh dry-run $(if $(filter command line,$(origin VERSION)),$(VERSION),v0.0.0-dryrun)

tidy: ## go mod tidy
	$(GO) mod tidy

clean: ## Remove build output
	rm -rf bin dist
