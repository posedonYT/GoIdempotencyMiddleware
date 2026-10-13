.PHONY: help vet test test-race test-cover bench lint fmt check scratch

GO ?= go
GOLANGCI_LINT ?= golangci-lint

help:
	@echo "Targets:"
	@echo "  make vet         go vet ./..."
	@echo "  make test        go test ./..."
	@echo "  make test-race   go test -race -count=1 ./..."
	@echo "  make test-cover  go test -cover ./..."
	@echo "  make bench       go test -bench=. -benchmem ./..."
	@echo "  make lint        golangci-lint run ./..."
	@echo "  make fmt         gofmt + goimports (if installed)"
	@echo "  make check       vet + test-race + lint"
	@echo "  make scratch     create scratch/ for stage-1 experiments"

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

test-race:
	$(GO) test -race -count=1 ./...

test-cover:
	$(GO) test -cover ./...

bench:
	$(GO) test -bench=. -benchmem ./...

lint:
	$(GOLANGCI_LINT) run ./...

fmt:
	$(GO) fmt ./...
	@command -v goimports >/dev/null 2>&1 && goimports -w . || true

check: vet test-race lint

scratch:
	@mkdir -p scratch
	@echo "scratch/ ready (ignored by git; plan stage 1)"
