# Antigravity Gateway (AGW) Makefile

.PHONY: all help build run clean fmt fmt-check lint test test-short test-integration test-all check release hooks-install pre-commit

# Default target: build binary
all: build

# Show available targets
help:
	@echo "Antigravity Gateway (AGW) build targets:"
	@echo "  make build             - Build agw CLI binary (bin/agw)"
	@echo "  make run               - Run agw in foreground (agw start)"
	@echo "  make clean             - Remove build artifacts (bin/)"
	@echo "  make fmt               - Format all Go files with gofmt"
	@echo "  make fmt-check         - Check Go formatting without modifying files"
	@echo "  make lint              - Run static analysis with go vet"
	@echo "  make test              - Run unit tests with race detection"
	@echo "  make test-short        - Run fast unit tests"
	@echo "  make test-integration  - Run end-to-end integration tests"
	@echo "  make test-all          - Run both unit and integration tests"
	@echo "  make check             - Run full preflight verification (fmt-check, lint, test)"
	@echo "  make release           - Package cross-platform release archives with checksums (dist/)"
	@echo "  make hooks-install     - Configure git pre-commit hook via core.hooksPath"
	@echo "  make pre-commit        - Run pre-commit checks manually"

build:
	@echo "[build] building agw binary"
	@mkdir -p bin
	@go build -o bin/agw ./cmd/agw

run:
	@echo "[run] starting agw daemon in foreground"
	@go run ./cmd/agw start

clean:
	@echo "[clean] removing build artifacts"
	@rm -rf bin dist

fmt:
	@echo "[fmt] formatting Go code with gofmt"
	@gofmt -w .

fmt-check:
	@echo "[fmt-check] verifying Go formatting"
	@UNFORMATTED=$$(gofmt -l .); \
	if [ -n "$$UNFORMATTED" ]; then \
		echo "[fmt-check] ERROR: unformatted Go files detected:"; \
		echo "$$UNFORMATTED" | sed 's/^/  - /'; \
		echo "[fmt-check] run 'make fmt' to format them"; \
		exit 1; \
	fi

lint:
	@echo "[lint] running static analysis (go vet)"
	@go vet ./...

test:
	@echo "[test] running unit tests with race detector"
	@go test -v -race ./...

test-short:
	@echo "[test-short] running fast unit tests"
	@go test -short ./...

test-integration:
	@echo "[test-integration] running end-to-end integration tests"
	@go test -v -race -tags=integration ./test/integration/...

test-all: test test-integration

check: fmt-check lint test

release:
	@echo "[release] packaging cross-platform release artifacts with checksums"
	@./scripts/build-release.sh

hooks-install:
	@./scripts/install-hooks.sh

pre-commit:
	@.githooks/pre-commit
