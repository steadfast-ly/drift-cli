# drift CLI

Command-line client for [drift](docs/DESIGN.md), the preview-environment and
release-management service. One binary talks to every deployment.

Status: **v0.1** — the read slice. Environments list and get, contexts,
paste-based login, diagnostics.

## Install

```sh
mise use -g github:steadfast/drift-cli
```

or build from source:

```sh
make build && ./drift version
```

## Getting started

```sh
drift context add au --endpoint https://drift.example.com
drift auth login        # prints the credentials page URL, prompts for the token
drift doctor            # reachability, VPN, credential, skew, capabilities
drift env list
```

## Commands

```
drift auth        login | logout | status
drift context     list | use <name> | current | add <name> | remove <name>
drift env         list | get <slug-or-id>
drift doctor      reachability, VPN, auth validity, version skew, capabilities
drift version     client, API contract, server and API versions, context
drift completion  bash | zsh | fish
```

Environments are addressed by **slug or UUID** and the server resolves both. A
slug resolves only environments that are neither `destroyed` nor `canceled`, so
a slug reused over time addresses the one live environment holding it; address a
torn-down environment by id.

## Output

Table by default. `-o json|yaml|wide` selects a format; `--json <fields>`
projects onto a stable, CLI-owned field contract that does not move when the
server renames a wire property.

```sh
drift env list -o json | jq '.items[] | select(.status == "running")'
drift env list --json slug,status,expires
drift env list -o wide
```

Data goes to **stdout**, diagnostics to **stderr**, so `drift env list -o json >
envs.json` produces a file that parses.

Colour honours `NO_COLOR`, `TERM=dumb`, `CLICOLOR_FORCE` and CI detection.
Formatting is the only thing that changes off a TTY — same rows, same columns,
same widths.

## Exit codes

| Code | Meaning |
| ---- | ------- |
| 0 | success |
| 1 | error |
| 2 | usage — bad flags or arguments |
| 3 | not found |
| 4 | authentication required |
| 5 | state conflict or operation failed |
| 6 | wait timed out |
| 7 | rate limited — allocated, not yet emitted |

They are a public contract; `drift --help` carries the same table.

## Credential scoping

A stored credential is sent **only** to the endpoint its own context names.

`--context` and `--endpoint` are independent overrides, so a current context of
`prod` combined with `--endpoint http://elsewhere` names a context *and* a
foreign address. Attaching the context's token there would hand over the
operator's whole authority on `prod` to whatever was typed, and a typo, a copied
command line or a hostile README is enough. So the CLI refuses, and says so.

To authenticate against an ad-hoc endpoint, set `DRIFT_TOKEN` — exporting a
token is an explicit act naming a credential for what runs next. To make it
permanent, give it a context of its own.

The same rule runs the other way: `drift auth login` will not file a credential
minted at one deployment under a context that points at another.

`drift doctor` reports when a credential is in scope and when it is being
withheld.

## Configuration

`$XDG_CONFIG_HOME/drift/config.yaml` holds contexts and defaults and **no
secrets**, so it is safe to commit to a dotfiles repository.

```yaml
current-context: au
contexts:
  - name: au
    endpoint: https://drift.example.com
```

Credentials are stored separately: the OS keyring where one is available, a
0600 `credentials.yaml` beside the config on headless Linux. `drift auth login`
reports which was used.

Each credential is filed under its **context name and endpoint**, so two config
files that both call a context `prod` cannot share one entry — the storage-layer
form of the same rule. Re-pointing a context at a different deployment clears
the credential filed against the old address, since it would otherwise be
unreachable through both `auth logout` and `context remove`. A credential written by an earlier build under the bare context name is **not**
reused: the OS keyring is global to the user account, so such an entry cannot be
shown to belong to any particular deployment, and adopting it would hand one
config directory a credential minted for another. `drift auth login` once
re-files it; the CLI says so rather than looking like it logged itself out, and
`drift auth logout` removes the old entry. A legacy entry in the credential
*file* is migrated automatically — that file belongs to one config directory, so
there is nothing to confuse it with.

If a credential is rotated while the OS keyring is unreachable — over SSH, say —
and the keyring later comes back holding something different, drift reports the
conflict instead of guessing which is current. `drift auth login` or
`drift auth logout` settles it.

`DRIFT_TOKEN` overrides stored credentials entirely and suppresses interactive
login — the CI path.

Override precedence: **flag > environment variable > current context.**

| Setting | Flag | Environment |
| ------- | ---- | ----------- |
| context | `--context` | `DRIFT_CONTEXT` |
| endpoint | `--endpoint` | `DRIFT_ENDPOINT` |
| output | `-o` / `--output` | `DRIFT_OUTPUT` |
| credential | — | `DRIFT_TOKEN` |
| config directory | — | `DRIFT_CONFIG_DIR`, `XDG_CONFIG_HOME` |

## Capability discovery

Every deployment serves `/.well-known/drift.json`. The CLI fetches it per
context and caches it with an ETag, revalidating rather than expiring. It
carries the organisation, the server version, whether SSO is enforced, where the
API lives, which features exist, and the minimum client version.

Commands for absent features still appear in `--help` — a help text that changes
per server is undiscoverable — and fail fast on invocation, naming the context
and the server version.

**Version skew warns loudly and never refuses.** A hard floor would brick an
operator's CLI in the middle of an incident, which is precisely when it matters
most.

The document decides where credentialled requests go, so `services["api.v1"]`
is validated rather than trusted: anything that is not a single rooted path is
refused and `/api/v1` used instead, the joined URL is re-parsed and asserted to
still address the configured host, and redirects are refused outright. The
validation is applied on cache reads as well as on fetch, so a document that was
poisoned once cannot survive behind a 304.

## What v0.1 does not deliver

Recorded against [`docs/DESIGN.md`](docs/DESIGN.md) so the gaps are not mistaken
for finished work:

- **One vendored spec, not one per org.** §4 calls for both forks' specs to be
  vendored, with a conformance test asserting each server's spec is a superset
  of the CLI's pinned contract — described there as *the only durable guard*
  against the two forks diverging on the shared surface. Only `make
  check-generated` exists today, which proves the committed client matches the
  committed spec and says nothing about any server.
- **The vendored spec tracks an unmerged branch.** `spec/openapi.json` was taken
  from a server working branch, not from a released deployment. It must be
  re-vendored from the merged contract before any release is cut.
- **`auth status` cannot report the credential's owner, role or expiry.**
  `/api/v1` has no operation describing the calling credential, so the command
  reports what it can prove and points at the server's credentials page for the
  rest.
- **Reads only.** Environment mutations, releases, promotions, repositories,
  audit log, `drift api` and wait semantics are steps 4-6 of §7.
- **Exit code 6 is reserved but unreachable** — no v0.1 command waits.
- **429 has no distinct exit code.** Nothing emits one until per-credential rate
  limiting lands (§7 step 5); see `internal/cliexit.FromProblem` for the
  recommendation when it does.

## Development

```sh
make            # fmt-check, vet, check-generated, test
make generate   # regenerate internal/api from spec/openapi.json
make tools      # install the pinned oapi-codegen
```

`internal/api` is **generated** from `spec/openapi.json` by
[`oapi-codegen`](https://github.com/oapi-codegen/oapi-codegen) v2.8.0 and is
never hand-edited. `make check-generated` runs in CI and fails the build if the
committed client and the vendored spec disagree.

The servers are VPN-gated and this repository's CI can reach neither, so the
contract travels as a committed artifact:

```sh
SERVER_REPO=/path/to/a/drift/checkout make vendor-spec
```

Golden output files live in `internal/output/testdata`. Regenerate them
deliberately, and read the diff:

```sh
make test-update-golden
```
