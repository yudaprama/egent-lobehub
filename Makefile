VERSION ?= dev
LDFLAGS  = -s -w -X main.version=$(VERSION)
GO       = go

.PHONY: build build-all test clean run

build: ## Build for current platform
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/egent-lobehub .

build-all: ## Cross-compile for all targets
	@mkdir -p dist
	for os in linux darwin; do \
	  for arch in amd64 arm64; do \
	    echo "  building $$os/$$arch..."; \
	    GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 \
	      $(GO) build -trimpath -ldflags "$(LDFLAGS)" \
	        -o "dist/egent-lobehub-$$os-$$arch" .; \
	    tar -czf "dist/egent-lobehub-$$os-$$arch.tar.gz" \
	      -C dist "egent-lobehub-$$os-$$arch"; \
	    rm "dist/egent-lobehub-$$os-$$arch"; \
	  done; \
	done
	ls -lh dist/

test: ## Run all tests
	$(GO) test ./...

run: build ## Build and run
	./bin/egent-lobehub $(ARGS)

version: ## Print version
	@$(GO) run -ldflags "$(LDFLAGS)" . -version

clean: ## Remove build artifacts
	rm -rf bin/ dist/

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?##' Makefile | sort | \
	  awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-15s\033[0m %s\n", $$1, $$2}'
