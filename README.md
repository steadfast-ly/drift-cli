# drift CLI

Command-line client for [drift](https://steadfast-ly.github.io/drift-cli/),
the preview-environment and release-management service. One binary talks to
every deployment.

## Install

```sh
mise use -g github:steadfast-ly/drift-cli
```

Or with Go:

```sh
go install github.com/steadfast-ly/drift-cli@latest
```

Or download a binary from
[GitHub Releases](https://github.com/steadfast-ly/drift-cli/releases).

## Quickstart

```sh
# Point the CLI at your deployment.
drift context add prod --endpoint https://drift.example.com

# Mint a credential at <endpoint>/credentials, then paste it.
drift auth login

# Verify connectivity, auth and version skew.
drift doctor

# Create a preview environment from the current branch.
drift env create
```

## Documentation

**[steadfast-ly.github.io/drift-cli](https://steadfast-ly.github.io/drift-cli/)**
-- the full manual: install, concepts, guides, command reference,
troubleshooting.

## Development

```sh
make            # fmt-check, vet, check-generated, test, test-race
make generate   # regenerate internal/api from spec/openapi.json
make tools      # install the pinned oapi-codegen
make docs-gen   # generate command-reference pages
make docs-build # build the docs site (requires mkdocs-material)
```

`internal/api` is **generated** from `spec/openapi.json` by
[`oapi-codegen`](https://github.com/oapi-codegen/oapi-codegen) v2.8.0 and is
never hand-edited. `make check-generated` runs in CI and fails the build if
the committed client and the vendored spec disagree.

Golden output files live in `internal/output/testdata`. Regenerate them
deliberately, and read the diff:

```sh
make test-update-golden
```

## Licence

Apache-2.0 -- see [`LICENSE`](LICENSE).

## Contributing

See [`CONTRIBUTING.md`](CONTRIBUTING.md). External contributions are not
accepted.
