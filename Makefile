# drift CLI.
#
# `internal/api` is GENERATED from `spec/openapi.json` and must never be
# hand-edited. `make generate` is the only sanctioned way to change it, and
# `make check-generated` is the CI gate that fails the build when the committed
# code and the vendored spec disagree — the failure mode that would otherwise
# surface as a runtime decode error against a real server.

GO            ?= go
OAPI_CODEGEN  ?= $(shell command -v oapi-codegen 2>/dev/null || echo $(shell $(GO) env GOPATH)/bin/oapi-codegen)
OAPI_VERSION  := v2.8.0
SPEC          := spec/openapi.json
GENERATED     := internal/api/client.gen.go
VERSION       ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev)
LDFLAGS       := -X github.com/steadfast-ly/drift-cli/cmd.Version=$(VERSION)

.DEFAULT_GOAL := check

.PHONY: check
check: fmt-check vet check-generated test test-race ## everything CI runs

.PHONY: build
build: ## build ./drift
	$(GO) build -ldflags "$(LDFLAGS)" -o drift .

.PHONY: install
install:
	$(GO) install -ldflags "$(LDFLAGS)" .

.PHONY: test
test:
	$(GO) test ./...

# The credential file holds every context, so a write is a read-modify-write
# over shared state and the concurrency tests are the ones that matter here.
.PHONY: test-race
test-race:
	$(GO) test -race ./...

.PHONY: test-update-golden
test-update-golden: ## rewrite the table/JSON golden files
	$(GO) test ./internal/output -update

.PHONY: vet
vet:
	$(GO) vet ./...

.PHONY: fmt
fmt:
	gofmt -w .

.PHONY: fmt-check
fmt-check:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt would change:"; echo "$$unformatted"; exit 1; \
	fi

.PHONY: tools
tools: ## install the pinned code generator
	$(GO) install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@$(OAPI_VERSION)

# The generator's OUTPUT changes between releases, so `check-generated` compares
# committed code against whatever generator happens to be on PATH. A newer one
# produces a spurious diff that looks like contract drift and sends someone
# hunting a change nobody made; an older one hides a real one. Assert the
# version before either target runs.
.PHONY: check-codegen-version
check-codegen-version:
	@test -x "$(OAPI_CODEGEN)" || { echo "oapi-codegen not found; run 'make tools'"; exit 1; }
	@found=$$("$(OAPI_CODEGEN)" --version | tail -1); \
	if [ "$$found" != "$(OAPI_VERSION)" ]; then \
		echo "ERROR: oapi-codegen $$found found, but this repo pins $(OAPI_VERSION)."; \
		echo "       Run 'make tools' to install the pinned version."; \
		exit 1; \
	fi

.PHONY: generate
generate: check-codegen-version ## regenerate internal/api from the vendored spec
	@echo "==> oapi-codegen $(OAPI_VERSION)"
	cd internal/api && $(OAPI_CODEGEN) --config oapi-codegen.yaml ../../$(SPEC)
	gofmt -w $(GENERATED)

# The gate. Regenerates into a scratch copy and diffs; a mismatch means either
# the spec was revendored without regenerating, or the generated file was
# hand-edited. Both are the same failure: the committed client no longer
# describes the committed contract.
.PHONY: check-generated
check-generated: check-codegen-version
	@tmp=$$(mktemp -d); \
	cp internal/api/oapi-codegen.yaml $$tmp/; \
	(cd $$tmp && "$(abspath $(OAPI_CODEGEN))" --config oapi-codegen.yaml "$(abspath $(SPEC))" >/dev/null); \
	gofmt -w $$tmp/client.gen.go; \
	if ! diff -u $(GENERATED) $$tmp/client.gen.go > $$tmp/drift.diff; then \
		echo "ERROR: $(GENERATED) does not match $(SPEC)."; \
		echo "       Run 'make generate' and commit the result."; \
		head -60 $$tmp/drift.diff; \
		rm -rf $$tmp; exit 1; \
	fi; \
	rm -rf $$tmp; \
	echo "generated client matches $(SPEC)"

# Revendor the contract from a server checkout, then regenerate. The servers are
# VPN-gated and this repository's CI can reach neither, so the spec travels as a
# committed artifact rather than as a live fetch (DESIGN.md §4).
.PHONY: vendor-spec
vendor-spec: ## SERVER_REPO=/path/to/drift make vendor-spec
	@test -n "$(SERVER_REPO)" || { echo "set SERVER_REPO=/path/to/a/drift/checkout"; exit 1; }
	cp "$(SERVER_REPO)/openapi.json" $(SPEC)
	$(MAKE) generate

.PHONY: clean
clean:
	rm -f drift

.PHONY: help
help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'
