.PHONY: all build test bench run clean docker lint dashboard fmt vet

GO       := go
BINARY   := bin/meridian
GOFLAGS  := -ldflags="-s -w"
PACKAGES := ./...

all: build dashboard

# ── Go ───────────────────────────────────────────────────────────────

build:
	$(GO) build $(GOFLAGS) -o $(BINARY) ./cmd/meridian

test:
	$(GO) test $(PACKAGES) -v -race -count=1

bench:
	$(GO) test $(PACKAGES) -bench=. -benchmem -run=^$$

vet:
	$(GO) vet $(PACKAGES)

fmt:
	gofmt -s -w .

lint: vet
	@echo "Lint passed"

# ── Dashboard ────────────────────────────────────────────────────────

dashboard:
	cd dashboard && npm install --no-audit --no-fund && npx vite build

dashboard-dev:
	cd dashboard && npm install --no-audit --no-fund && npx vite

# ── Run ──────────────────────────────────────────────────────────────

run: build
	./$(BINARY) serve

demo: build dashboard
	./run.sh demo

simulate: build
	./$(BINARY) simulate

query: build
	./$(BINARY) query "$(Q)"

# ── Docker ───────────────────────────────────────────────────────────

docker:
	docker build -t meridian:latest .

docker-up:
	docker compose up --build

docker-down:
	docker compose down -v

# ── Cleanup ──────────────────────────────────────────────────────────

clean:
	rm -rf bin/ data/ dashboard/dist dashboard/node_modules
	$(GO) clean -testcache
