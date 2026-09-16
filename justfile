# drift CLI.
#
# `internal/api` is GENERATED from `spec/openapi.json` and must never be
# hand-edited. `just generate` is the only sanctioned way to change it, and
# `just check-generated` is the CI gate that fails the build when the committed
# code and the vendored spec disagree — the failure mode that would otherwise
# surface as a runtime decode error against a real server.

GO           := env_var_or_default("GO", "go")
# The pinned code generator, resolved at runtime: whatever is on PATH, falling
# back to GOPATH/bin. `just check-codegen-version` refuses to run against
# anything that is not the pinned version.
OAPI_CODEGEN := `command -v oapi-codegen 2>/dev/null || echo "$(go env GOPATH)/bin/oapi-codegen"`
OAPI_VERSION := "v2.8.0"
SPEC         := "spec/openapi.json"
GENERATED    := "internal/api/client.gen.go"
VERSION      := env_var_or_default("VERSION", `git describe --tags --always --dirty 2>/dev/null || echo dev`)

# List the available recipes
default:
    @just --list

# Everything CI runs: format check, vet, the generated-client gate, the full
# test suite under the race detector, and the build. The suite executes
# exactly ONCE here -- race coverage is a superset of plain coverage, so a
# separate plain run would only double the most expensive step (issue #10).
# `just test` and `just test-race` stay as standalone recipes for local use.
check: fmt-check vet check-generated test-race build

# Build ./drift with the version ldflags baked in
build:
    {{ GO }} build -ldflags "-X github.com/steadfast-ly/drift-cli/cmd.Version={{ VERSION }}" -o drift .

# Install the CLI into GOBIN with the version ldflags baked in
install:
    {{ GO }} install -ldflags "-X github.com/steadfast-ly/drift-cli/cmd.Version={{ VERSION }}" .

test:
    {{ GO }} test ./...

# The credential file holds every context, so a write is a read-modify-write
# over shared state and the concurrency tests are the ones that matter here.
test-race:
    {{ GO }} test -race ./...

# Rewrite the table/JSON golden files
test-update-golden:
    {{ GO }} test ./internal/output -update

vet:
    {{ GO }} vet ./...

fmt:
    gofmt -w .

fmt-check:
    #!/usr/bin/env bash
    set -euo pipefail
    unformatted="$(gofmt -l .)"
    if [ -n "$unformatted" ]; then
        echo "gofmt would change:"
        echo "$unformatted"
        exit 1
    fi

# Install the pinned code generator
tools:
    {{ GO }} install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@{{ OAPI_VERSION }}

# The generator's OUTPUT changes between releases, so `check-generated` compares
# committed code against whatever generator happens to be on PATH. A newer one
# produces a spurious diff that looks like contract drift and sends someone
# hunting a change nobody made; an older one hides a real one. Assert the
# version before either target runs.
check-codegen-version:
    #!/usr/bin/env bash
    set -euo pipefail
    if [[ ! -x "{{ OAPI_CODEGEN }}" ]]; then
        echo "oapi-codegen not found; run 'just tools'" >&2
        exit 1
    fi
    found="$("{{ OAPI_CODEGEN }}" --version | tail -1)"
    if [[ "$found" != "{{ OAPI_VERSION }}" ]]; then
        echo "ERROR: oapi-codegen $found found, but this repo pins {{ OAPI_VERSION }}." >&2
        echo "       Run 'just tools' to install the pinned version." >&2
        exit 1
    fi

# Regenerate internal/api from the vendored spec
generate: check-codegen-version
    @echo "==> oapi-codegen {{ OAPI_VERSION }}"
    cd internal/api && "{{ OAPI_CODEGEN }}" --config oapi-codegen.yaml ../../{{ SPEC }}
    gofmt -w {{ GENERATED }}

# The gate. Regenerates into a scratch copy and diffs; a mismatch means either
# the spec was revendored without regenerating, or the generated file was
# hand-edited. Both are the same failure: the committed client no longer
# describes the committed contract.
check-generated: check-codegen-version
    #!/usr/bin/env bash
    set -euo pipefail
    root="$(pwd)"
    tmp="$(mktemp -d)"
    cp internal/api/oapi-codegen.yaml "$tmp/"
    (cd "$tmp" && "{{ OAPI_CODEGEN }}" --config oapi-codegen.yaml "$root/{{ SPEC }}" >/dev/null)
    gofmt -w "$tmp/client.gen.go"
    if ! diff -u {{ GENERATED }} "$tmp/client.gen.go" > "$tmp/drift.diff"; then
        echo "ERROR: {{ GENERATED }} does not match {{ SPEC }}." >&2
        echo "       Run 'just generate' and commit the result." >&2
        head -60 "$tmp/drift.diff"
        rm -rf "$tmp"
        exit 1
    fi
    rm -rf "$tmp"
    echo "generated client matches {{ SPEC }}"

# Revendor the contract from a server checkout, then regenerate. The servers are
# VPN-gated and this repository's CI can reach neither, so the spec travels as a
# committed artifact rather than as a live fetch (see CONTRIBUTING.md). For
# server releases, .github/workflows/spec-sync.yaml automates this path.
#
# `just vendor-spec /path/to/drift/checkout`
vendor-spec SERVER_REPO:
    cp "{{ SERVER_REPO }}/openapi.json" {{ SPEC }}
    just generate

# Generate the command-reference pages
docs-gen:
    {{ GO }} run ./tools/docsgen

# Build docs and serve locally
docs-serve: docs-gen
    uvx --with mkdocs-material mkdocs serve

# Build the docs site
docs-build: docs-gen
    uvx --with mkdocs-material mkdocs build --strict

# Remove build output and generated docs
clean:
    rm -f drift
    rm -rf site/ docs/reference/commands/

# ---------------------------------------------------------------------------
# Release
#
# A release is an ANNOTATED semver tag pushed to origin -- NOTHING releases on
# a merge to main. `.github/workflows/release.yaml` matches v[0-9]+.[0-9]+.[0-9]+
# on tag push and builds the versioned binaries at that one version; its
# preflight refuses a version that already exists, so tags are IMMUTABLE:
# repair a bad release by cutting the NEXT version, never by re-tagging.
#
# The tag targets origin/main's freshly-fetched HEAD, never the local
# checkout, so a stale or dirty worktree cannot release unpushed code.
#
# A tag target whose commit message carries a GitHub skip marker is REFUSED:
# GitHub applies those markers from the tagged commit's message to tag-push
# events, so such a tag never starts the Release workflow -- the tag burns
# silently (three burned tags on drift; issue #9).
#
# Server spec bumps arrive as spec-sync PRs (CONTRIBUTING.md); merge those
# BEFORE cutting a release that should carry the new contract.
# ---------------------------------------------------------------------------

# Cut release vX.Y.Z: verify, tag origin/main, push (the tag starts the build)
release version:
    #!/usr/bin/env bash
    set -euo pipefail
    v="{{ version }}"; v="${v#v}"
    if [[ ! "$v" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
        echo "usage: just release X.Y.Z (got '{{ version }}')" >&2; exit 1
    fi
    git fetch origin
    if git rev-parse -q --verify "refs/tags/v$v" > /dev/null \
       || git ls-remote --exit-code --tags origin "refs/tags/v$v" > /dev/null 2>&1; then
        echo "error: v$v already exists -- tags are immutable, cut the next version" >&2
        exit 1
    fi
    target="$(git rev-parse origin/main)"

    # GitHub applies skip markers from the TAGGED COMMIT's message to tag-push
    # events too, so a release tagged on a commit carrying one never starts
    # the Release workflow -- the tag burns silently. Scan the FULL message
    # (subject + body), case-insensitively: GitHub matches the tokens
    # anywhere, quoted or disclaimed.
    msg="$(git log -1 --format=%B "$target")"
    if grep -qiE '\[(skip ci|ci skip|no ci|skip actions|actions skip)\]' <<<"$msg"; then
        echo "error: the tag target's commit message suppresses tag workflows" >&2
        echo "       $(git log -1 --format='%h %s' "$target")" >&2
        echo "       land a workflow-eligible commit first, then re-run: just release $v" >&2
        exit 1
    fi

    echo "==> previous release: $(git tag --sort=-v:refname | head -1)"
    echo "==> v$v will tag origin/main:"
    git log -1 --oneline "$target"
    read -r -p "==> push tag v$v? [y/N] " answer
    [[ "$answer" == [yY]* ]] || { echo "aborted; nothing pushed"; exit 1; }
    git tag -a "v$v" -m "v$v" "$target"
    git push origin "v$v"
    echo "==> v$v pushed; the release workflow is running under GitHub Actions"
