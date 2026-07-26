# drift CLI

Command-line client for [drift](docs/DESIGN.md), the preview-environment and
release-management service. One binary talks to every deployment.

Status: **v0.2** — the write surface. The full environment lifecycle, rc and
hotfix promotions, and `--wait` on top of v0.1's reads, contexts, paste-based
login and diagnostics.

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
drift env         list | get | create | rm | cancel | relaunch | sleep | wake |
                  extend | share | unshare | add-service | remove-service |
                  swap-branch | retry-build | wait
drift release     status | history | promote rc | promote hotfix
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
| 7 | rate limited — retry after the interval the server names |

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

## Creating an environment

`drift env create` infers what it can from the directory you are standing in:

| Field | Inferred from |
| ----- | ------------- |
| repository | the `origin` remote, in any form git stores it |
| branch | `HEAD` |
| slug | the branch name, lower-cased and hyphenated to the server's pattern |
| ticket | an issue key in the branch name, upper-cased |
| PR number, title, URL | `gh pr list --head <branch> --json number,title,url` |

Every field has a flag that overrides it, and `--repo name:branch` repeats for a
multi-service environment. Repository names — `owner/name`, the bare name, the
display name or the helm chart key — are resolved to ids client-side against the
server's repository list; an ambiguous name is refused rather than guessed.

Inference **degrades to nothing** rather than to a wrong answer. A missing `gh`,
a detached HEAD, or a remote that is not a GitHub URL each produce a note on
stderr and leave the field unset. Outside a git repository, or when the session
is not interactive, nothing is inferred at all and `--slug` and `--repo` are
required — inference is a convenience for a human who can see the plan and say
no, and a script should say what it means.

The whole of inference runs under a five-second deadline — every failure already
degrades to "not inferred", so a `gh` blocked on a dead network costs seconds
rather than hanging the command.

The plan is printed before anything is created, with the inferred fields marked
as such. `--yes` waives the *question*, not the disclosure: the plan is still
written to stderr, because acting on four guessed fields without showing them is
the one thing the operator cannot check afterwards.

```
Create environment:
  slug     aus-10151-grid-pushdown   (inferred)
  ticket   AUS-10151   (inferred)
  service  acme/proofsvc @ AUS-10151-grid-pushdown   (inferred)
  pr       #4633 AUS-10151 grid SQL pushdown   (inferred)
  ttl      48h (server default)
Create this environment? [y/N]
```

## Wait semantics

Commands that start work **block by default**; commands that end it return
immediately. `--wait` and `--no-wait` override, and `drift env wait <ref> --for
<state>` follows either afterwards.

| Command | Default | Goal | Timeout |
| ------- | ------- | ---- | ------- |
| `create`, `relaunch`, `retry-build`, `add-service`, `swap-branch` | blocks | `running` | 30m |
| `wake` | blocks | `running` | 10m |
| `promote rc`, `promote hotfix` | blocks | promotion `completed` | 20m |
| `rm` | returns | `destroyed` | 20m |
| `sleep` | returns | `sleeping` | 10m |
| `cancel` | returns | `canceled` | 5m |

`--wait-timeout 0` means "this command's default" everywhere, including on the
promotions.

`rm` does not block because destroy convergence is owned by a cron with an
escalation window and routinely exceeds ten minutes; when it *is* waited on, the
timeout is sized for that.

**Failure is decided from the state machine, not from the state's name.**
`deploy_failed` is *not* terminal — the server documents `deploying ->
deploy_failed -> running` as an expected ArgoCD blip, and both `_failed` states
have server-raised recovery edges out of them. So a failure state is believed
only once it has persisted for **30 seconds**, measured on the clock rather than
counted in polls: a poll count silently ties the tolerance to the poll interval,
and recovery arrives on an ArgoCD webhook whose latency the CLI does not
control. A build still running is positive evidence of recovery and restarts the
window — though note that only helps `build_failed`, since `deploy_failed` is
entered after the builds have completed. A promotion is different, and is
treated differently: its `failed` and `deploy_failed` are declared *final* in its
own machine, so one observation is enough.

**A goal that is provably unreachable fails in seconds** rather than burning the
timeout: `drift env wait x --for running` on a sleeping environment answers at
once, because no server-raised edge leads out of `sleeping` and nothing here
asked for one. The CLI is deliberately conservative about that claim and makes
it only when it can be proven, so it does *not* apply when this invocation
issued the command itself (`rm --wait` reading a stale `running` once is not
proof), nor for `sleeping` and `canceled`, which have no inbound server-raised
edge at all — the absence of a path to them says nothing about whether a command
is on its way. In those cases a stalled wait ends as a timeout, which is honest.

A state the server reports that this build does not know is reported as version
skew, not reasoned about: every rule would otherwise answer from an empty edge
list and invent its conclusion.

Progress goes to **stderr** — animated on a terminal, one line per state change
everywhere else, so a CI log gets a transition history rather than three hundred
identical lines. Timing out is exit 6 and rolls nothing back.

## Rate limiting

`/api/v1` limits per credential and answers 429 with `Retry-After` in whole
seconds. That is exit **7**, with the server's interval in the hint.

A **mutation** is never retried automatically: a write is a deliberate act, and
silently repeating one is worse than reporting it. A **`--wait` poll loop** does
back off — by the server's own number where that asks for more patience than the
CLI already had — and carries on; the wait's deadline still applies, so a
persistently throttled wait ends as a timeout rather than spinning.

The backoff is bounded on both sides. It never drops below the poll interval,
because a client that has just been told it is sending too many requests must
not come back *sooner* than it would have anyway; and a `Retry-After` too large
to be meaningful is discarded rather than multiplied, since `time.Duration` is
int64 nanoseconds and a big enough value wraps to a short one. `Retry-After` in
the RFC 9110 HTTP-date form — legal, and what an ALB, WAF or CDN emits — is
converted to seconds rather than being allowed to fail the response parse.

A 429 that does not arrive as a drift envelope stays on the generic error path:
an intermediary shedding load is not drift rate-limiting the caller, and the two
have different remedies.

## What v0.2 does not deliver

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
- **The write commands need feature strings the server does not yet advertise.**
  `/.well-known/drift.json` still lists only the four read features, so capability
  gating refuses every mutation and promotion against a real deployment. The
  server's `FEATURES_SUPPORTED` has to gain `environments.write`, `promotions.rc`
  and `promotions.hotfix`.
- **`drift release promote prd` does not promote.** It explains that production
  promotion requires an elevated short-TTL credential and points at the web UI.
  Neither the operation nor the mint is on `/api/v1` (§7 step 6).
- **No `drift repo`, `drift audit` or `drift api`.** The contract has the
  operations; the commands are not built. Repository listing exists only as the
  name resolution behind `--repo`.
- **`auth status` still cannot report the credential's owner, role or expiry**,
  even though `GET /auth/whoami` now exists in the contract.
- **An inferred slug can collide silently.** Two branches that differ only past
  the 29-character limit produce the same slug. The server's uniqueness index
  among live environments catches it and the CLI turns that 409 into a hint
  naming the environment already holding the slug, so nothing is created against
  the wrong environment — but the CLI does not warn before the round trip.
- **A wedged sibling build masks a confirmed failure.** `BuildInFlight` restarts
  the failure window for as long as *any* build has an open outcome, so a build
  stuck in `queued` keeps a genuinely failed environment from being reported
  until the wait times out. Exit 6 rather than exit 5, with the state visible in
  the progress output.

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
