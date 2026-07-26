# Drift CLI — Design

Status: decided, revised after adversarial review.
Targets: `auditsight/drift`, `entromy/drift`, future org forks. One binary, all orgs.

## 1. Scope

In scope:

- Environments — full lifecycle: create, list, get, delete, cancel, relaunch, sleep, wake, extend, visibility, add/remove service, swap-branch, retry-build.
- Reads — release state (stg/rc image tags, commit SHAs, pod health), promotions active/history, repositories, branches, audit log.
- Promotions — rc, hotfix, and prd behind elevation.

Out of scope: repository CRUD, dependency DAG edges, application-group management. These stay web-only, which keeps the `admin` role off CLI credentials entirely.

## 2. Server-side contract layer

Constraint: Server Actions are not callable by non-browser clients. Action IDs are encrypted build artifacts and rotate at most every 14 days even when source is unchanged; `serverActions` config exposes only `allowedOrigins` and `bodySizeLimit`. Next.js docs designate Route Handlers as the external surface (Backend-for-Frontend guide, 16.2.11); the React Server Functions transport is explicitly outside semver.

Decision: **oRPC** (`@orpc/server`, pinned 1.14.x), one procedure definition per operation driving three surfaces:

- HTTP handler mounted at a catch-all Route Handler — `src/app/api/v1/[[...rest]]/route.ts`
- Server Action shell via `.actionable()` — web UI keeps current UX
- OpenAPI 3.1.1 document via `OpenAPIGenerator` — consumed by Go codegen

Gate: PASSED. The spike wired three operations of differing shape (a read, a simple mutation, a mutation with richer input), emitted OpenAPI 3.1.1, generated a Go client with `oapi-codegen` v2.8.0, built it, and ran it against a live Drift instance — round-tripping both a successful read and a typed 404. `pnpm typecheck` and `next build` both green. 3.1 consumed directly, no downconversion needed. Cost is ~2.4 MB unpacked, ~29 ms one-time cold init, ~0.3 ms warm overhead. Estimated full migration: 15-23 working days, with risk concentrated almost entirely in the action-layer normalization below.

Four findings from the spike that the plan must absorb:

- **`.actionable()` returns a `[error, data]` tuple, not `{ ok, error }`.** The web UI expects the latter. A per-action shim is mandatory, and it cannot be a generic helper — a `"use server"` file may only export async functions. Budget one small shim per migrated action.
- **Error envelopes must not double up.** Middleware runs before oRPC, so a naive mount makes `/api/v1` emit `ApiProblem` when unauthenticated and oRPC's own envelope otherwise. The generated Go client only knows the second, so it would fail to decode the most common CLI error. Decision: **`/api/v1` is added to the middleware public-bypass list and oRPC owns its entire error surface**, including authentication. Middleware keeps guarding pages and the legacy `/api/*` routes the web UI uses. One envelope per surface, and `/api/v1` becomes self-contained — which is also where bearer-token authentication has to live.
- **oRPC's error envelope is not overridable.** `ApiProblem` can only ride inside `data`. `customErrorResponseBodySchema` rewrites the emitted spec, not the wire, so it is easy to produce a document that lies about the response. Whatever shape is chosen must be verified against real wire output, not against the spec.
- **Naive spec output is unusable for codegen** — 42 error structs for 3 operations, anonymous domain structs, and `const`-without-`type` degrading discriminators to `interface{}`. Two fixes (`commonSchemas` plus `customErrorResponseBodySchema`) took the generated client from 3,024 to 1,147 lines with properly named types. Treat spec shaping as part of the work, not an afterthought.

Latent trap found: importing a constant from a `"use server"` file into the router is a hard `next build` failure. `MAX_TOTAL_TTL_HOURS` is currently duplicated at `environments.ts:502` to dodge exactly this.

Fallback had the spike failed: Hono + `@hono/zod-openapi`.

Rejected: ts-rest (Zod 4 support RC for 13+ months, OpenAPI 3.1 unshipped, maintenance stalled); tRPC + trpc-to-openapi (single-maintainer fork, hand-written action bridge); hand-rolled factory (same two-shell duplication, no ecosystem middleware).

### Prerequisite: action-layer normalization

The premise that existing cores drop in unmodified is false, verified against the code. Three things must change first, all independently justified:

- **Hoist authorization.** All 17 actions call `requireRole()` as their first statement, and it resolves the session internally via `auth()` (`src/lib/rbac.ts:69-96`). Actions must instead accept the resolved actor as a parameter, so authorization becomes real oRPC middleware rather than decoration. The spike confirmed this is not cosmetic: because `requireRole` resolves the session itself, oRPC middleware cannot see request headers, so **bearer tokens have no way in**. This is the single blocking dependency for the CLI, not a tidiness exercise.
- **Typed error union.** `Result<T, E>` is `E = Error` throughout, and `src/lib/environments.ts` returns bare `new Error("Environment not found")`, `new Error("Cannot sleep environment in X state")` and concurrency errors as the same type. Discriminating 404 from 409 from 500 currently requires string matching, which would have to stay byte-identical across two forks forever. Introduce `{ kind: "not_found" | "invalid_transition" | "conflict" | ... , state?, event? }` and the HTTP mapping becomes mechanical.
- **Lift inline schemas.** Several actions define input schemas inline (`promote-to-prd.ts:12`, `extend-environment.ts:17-27`) rather than in `src/schemas/`. Shared contracts must live in one place.

`create-environment.ts` is 508 lines of orchestration living in the action itself. Normalization does not require breaking it up, only changing how it receives its actor and reports failure.

Constraint discovered while hoisting authorization: cores must NOT live in `"use server"` modules. Every exported async function in such a module is registered by Next as a publicly callable RPC endpoint, so an exported core would be a live HTTP entry point whose only authorization is an actor object the caller supplies. Cores therefore live in `src/actions/core/`, and the shells in `src/actions/` export nothing but async action functions. Verified empirically: base and head builds each emit exactly 49 server-action ids with zero references under `core/`.

`SessionUser` is branded with a non-exported unique symbol implemented as a prototype getter, which blocks construction from a literal without requiring any `as` cast. What that does NOT block, verified by two independent agents with isolated `tsc` runs: derivation from an existing actor. All of `{...actor, role: "admin"}`, `Object.create(actor)`, `Object.assign({}, actor)`, `structuredClone(actor)`, `JSON.parse(JSON.stringify(actor))` and a single `claims as SessionUser` compile clean. TypeScript's spread result type carries the source's symbol keys, so a spread is *typed* as branded while being brand-less at runtime — a type-level lie in the permissive direction.

Consequence for the transport: a bearer path will rehydrate an identity from a verified token payload, and `const user: SessionUser = await req.json()` compiles with zero casts and zero review signal because `json()` returns `any`. So the brand alone is not the guarantee. `rbac.ts` must export an explicit mint that requires the caller to demonstrate it verified a credential, rather than merely assert a shape — otherwise the bearer path has no legitimate door and will use the invisible forgery instead of a visible cast. A guard test scanning production source for `as SessionUser` and for imports from `@/test/` backs it up, since the no-casts rule is convention only and Biome is on `recommended` alone.

### API conventions

- Versioned path prefix `/api/v1`. Additive-only within v1; breaking changes require `/api/v2`.
- Error envelope: versioned, RFC 9457-shaped `{ type, title, status, code, detail, instance, requestId }`. No CLI-facing standard exists — the envelope is ours and is versioned accordingly. It becomes implementable only after the typed error union above.
- HTTP mapping: 400 validation, 401 unauthenticated, 403 role, 404, 409 state-machine conflict, 429 rate limit, 500. 409 detail carries the current state and the rejected event, derived from `canTransition`.
- Pagination in the contract from v1 — `env list` and `audit list` both need it, and retrofitting is a breaking change. The DB layer is already limit/offset with a `limit + 1` over-fetch producing `hasMore`; the contract formalizes that.
- Environment addressing: users type slugs, the API takes UUIDs. Slug lookup is exposed server-side and states the collision rule explicitly — `getEnvironmentBySlug` (`src/lib/deps.ts:301-328`) excludes `destroyed` and `canceled`, so historical slugs recur and only the active environment resolves. Client-side list-and-filter is not acceptable.
- Long-running operations return the resource id immediately; clients poll. No SSE, matching existing UI behaviour.

Cut after review: `Idempotency-Key` infrastructure. Environment create is already guarded by slug uniqueness among active environments plus the namespace pre-flight check, and promotions are guarded by active-promotion state. 409 covers the real cases; revisit if a double-fire is ever observed.

### Prerequisite fixes (independently justified)

- Middleware returns 401 JSON for `/api/*` instead of 302 to an HTML sign-in page.
- `requireRole()` maps to 403 JSON instead of throwing a raw `Error` (currently a 500 HTML digest page).
- Read-role floor on the GET routes that lack any check: environment status, releases state, promotions active/history, repo branches. Note the middleware cookie gate already requires a session, so this adds a role floor and machine-readable failures, not access control from scratch.
- The two audit-log routes migrate onto the same seam, keeping their `preview` floor. Verified: a below-floor session there returned a bare 500 with an empty body.

Deferred from that change, tracked here so they are not lost:

- Non-auth error bodies (`{ error: "Invalid ID" }` and friends) still differ in shape from the problem envelope. Migrating them is the natural completion of the envelope work and should happen before the contract is published.
- `promotions/active` and `repos/[id]/branches` emit a naked 500 on DB failure — no content type, empty body. Pre-existing and present on `origin/main`; the sibling `history` route handles it. Same opaque-500 class the hardening exists to remove, and the CLI will hit it the moment the database hiccups.
- `audit-log/actors` returns a success-shaped `{ "actors": [] }` body with a 500 status, so a client cannot distinguish "no actors" from "query failed".
- `log.error("...", { err })` shorthand serialises an `Error` to `{}` because `message` and `stack` are non-enumerable and the logger uses `JSON.stringify`. Present at `history/route.ts:91` and `audit-log/route.ts:126`; worth a sweep across the repo.
- `WWW-Authenticate` is omitted from 401 responses for now. `Cookie` is not an IANA-registered auth scheme, so no conforming client can negotiate it. It returns as `Bearer` when the CLI credential lands, which is the point at which it becomes both correct and useful.

## 3. Authentication

Auth.js v5 is in maintenance mode (absorbed into Better Auth, announced 2025-09-26) and has no bearer, API-key, PAT or device-flow primitives. Better Auth is the destination.

The CLI credential is custom-minted regardless of framework, which is what makes the sequencing separable — see §7.

### The credential

Whatever mints it, the CLI stores a hashed-at-rest, role-snapshotted, expiring, revocable key — never a raw user session. Role is a snapshot capped at the user's live role at mint time, since IdP groups resolve only at sign-in. Kept honest by short TTLs, `lastUsedAt`, per-key revoke and revoke-all-for-user.

Storage on the client: OS keyring (`zalando/go-keyring`) with a 0600 file fallback for headless Linux, plus a `DRIFT_TOKEN` environment override that bypasses storage and suppresses interactive login.

### Minting, v1: web-minted key

`drift auth login` prints a URL, the browser authenticates through existing SSO, a page mints a scoped key, the user pastes it back. Zero new plugins, no device-code phishing surface, identical security properties to the device flow. Around 300 lines server-side.

### Minting, later: device flow

Once Better Auth lands, `drift auth login` upgrades to RFC 8628 device authorization — no paste, works over SSH and containers. This is UX polish on an already-working CLI, not a prerequisite.

Verified constraint that shapes it: `/device/token` returns `session.token` from an unmodified `createSession(user.id)` — a full user session with the user's entire authority on the global sliding TTL. There is no per-session TTL or permission capping. `@better-auth/api-key` has all of it (hashed at rest, per-key `expiresIn`, per-key `permissions`, `lastRequest`, list/update/delete) but the device flow does not mint API keys. So the device flow authenticates the human and our own code mints the scoped key — the same mint endpoint v1 already has, reached by a different door.

### Better Auth migration requirements

Carry-over requirements, with verified mapping:

- Hosted-domain gate — Better Auth's `hd` option is server-enforced against the signed ID token and rejects tokens with no `hd` claim when configured. Two parity gaps stay app-side: it passes open when `hd` is unconfigured (the existing fail-closed `superRefine` in `src/lib/config.ts` covers this, keep it), and `email_verified` is stored but not gated (add to the session hook).
- Google group resolution via Admin SDK Directory API — `google-groups.ts` ports essentially unchanged; it has no framework coupling. Wired into `databaseHooks.session.create.before`, which fires before every session insert and can abort by throwing `APIError`, preserving today's fail-closed `group_lookup_failed`. Groups persist as session `additionalFields` with `input: false`.
- Entra groups from the OIDC `groups` claim, same hook. Fallback if the raw profile is unreachable in hook context: parse the stored `account.idToken`, written before session creation.
- Role derivation from `DRIFT_ROLE_*_GROUP_ID`; `read-only` < `preview` < `release` < `admin`; default `read-only`. `resolveRole`, `hasRole`, `requireRole` and the `AUTH_ENABLED=false` synthetic admin survive untouched.
- Edge middleware — `getSessionCookie()` is presence-only and the docs label it insecure. Parity requires `getCookieCache(request, { secret })`, which verifies cryptographically without a DB call, at the cost of a staleness window equal to `cookieCache.maxAge`. Still strictly better than today's irrevocable 30-day JWTs.
- Version target 1.7 GA — v1.7.0-rc.2 carries a breaking `account` schema change (`accountId` to `providerAccountId`, `issuer` required). Adopting 1.6.x means a second migration within weeks. Pin exactly.

### Production promotion

`drift release promote prd` requires elevation: a browser round-trip minting a key scoped to `promote:prd` with a 15-minute TTL. A leaked long-lived token cannot reach production because it lacks the permission, and CI structurally cannot promote because minting requires interactive SSO.

Implemented as a parameterized re-login against the existing mint endpoint, not a bespoke elevation subsystem. The security property is identical; binding the grant to a calling token id adds nothing a 15-minute TTL does not already provide. Roughly a permission-set argument rather than the 300-500 lines a separate elevation table would cost.

## 4. Multi-org model

- Contexts, kubectl-style: a named bundle of endpoint, credential and defaults, with a current-context pointer. Override precedence: flag > environment variable > current context.
- Config at `$XDG_CONFIG_HOME/drift/config.yaml` (shareable); credentials stored separately. No multi-file merging (KUBECONFIG-style) — a documented footgun.

### Capability discovery

Each deployment serves `/.well-known/drift.json`, modelled on Terraform remote service discovery:

```
{
  "org": "auditsight",
  "version": "1.4.2",
  "auth": "sso" | "none",
  "services": { "api.v1": "/api/v1" },
  "features_supported": ["environments", "promotions.rc", "promotions.prd",
                         "promotions.hotfix", "application-groups", "audit-log"],
  "minimum_client_version": "0.3.0"
}
```

Cached per context with ETag. Commands for absent features appear in `--help` but fail fast on invocation, naming the context and server version. Unknown feature strings are ignored by older clients. `auth: "none"` covers `AUTH_ENABLED=false` servers (local development, reporter instances) — the CLI skips login entirely.

Version skew **warns loudly, does not refuse**. A hard floor means a server upgrade bricks operator CLIs mid-incident, which is precisely when the CLI matters most.

### Spec transport across orgs

Servers are VPN-gated and the CLI repo lives in a neutral organisation whose CI can reach neither. Each drift repo's CI generates and commits `openapi.json` in-repo as a build artifact; a sync step vendors both specs into the CLI repo. Conformance tests run against the vendored copies, asserting each server's spec is a superset of the CLI's pinned contract. This is the only durable guard against the two forks diverging on the shared surface.

## 5. CLI

Go, cobra v1.10.x, client generated by `oapi-codegen` v2.8.0 from the vendored OpenAPI 3.1.1 document. Generation runs in CI, so contract drift fails a build rather than surfacing at runtime.

```
drift-cli/
  cmd/                 cobra command tree
  internal/api/        GENERATED — do not edit
  internal/auth/       login, keyring, elevation
  internal/config/     contexts, discovery cache
  internal/output/     table | json | yaml | wide
  spec/                vendored openapi.json per org
  .goreleaser.yaml
```

### Command tree

Noun-verb grammar (`drift <noun> <verb>`), the 2026 default per clig.dev and Azure CLI guidelines. Canonical noun is **Environment**, CLI noun is `env`.

```
drift auth       login | logout | status | elevate | token list | token revoke
drift context    list | use <name> | current | add | remove
drift env        list | get | create | rm | cancel | relaunch | sleep | wake |
                 extend | share | unshare | add-service | remove-service |
                 swap-branch | retry-build | wait | open
drift release    status | history | promote rc|prd|hotfix
drift repo       list | branches
drift audit      list | actors
drift api        <method> <path>            raw escape hatch
drift completion bash | zsh | fish
drift version    client, server, api version, context
drift doctor     reachability, VPN, auth validity, version skew, capabilities
```

`env create` infers repo, branch, PR number/title/URL, slug and ticket from the working directory (git remote, HEAD, `gh`), with every field flag-overridable. Outside a git repo or on a non-TTY, explicit flags are required. Repository names resolve to UUIDs client-side.

`drift api` is governed by the same per-procedure server-side permission checks as every other command, keyed on the calling credential's capped permissions. Stated as an invariant: the escape hatch must not be a way around the scoped-key model.

### Conventions

- Output: table by default; `-o json|yaml|wide`; `--json <fields>` for a stable field contract. Data to stdout, diagnostics to stderr. No NDJSON streaming in v1 — no v1 command streams, and `-o json | jq` covers the need.
- Colour: honour `NO_COLOR`, `TERM=dumb`, `CLICOLOR_FORCE` and CI detection. Suppress animation when not a TTY; formatting is the only thing that changes off-TTY.
- Exit codes: 0 success, 1 error, 2 usage, 3 not found, 4 authentication required, 5 state conflict or operation failed, 6 wait timeout.
- Destructive operations (`rm`, `promote prd`, `relaunch`) confirm on a TTY, take `--yes`, and refuse without `--yes` when not a TTY.

### Wait semantics

Create, relaunch, wake, retry-build and promote block by default; delete, sleep and cancel return immediately. `--wait` / `--no-wait` override; `drift env wait --for <state> --timeout` exists for both.

Failure detection must respect the machine rather than the state name. `deploy_failed` is **not terminal** — `environment-machine.ts:125-126` defines `ARGOCD_HEALTHY → running` and `ARGOCD_PROGRESSING → deploying` as intentional recovery from transient ArgoCD Degraded blips, and CLAUDE.md documents `deploying → deploy_failed → running` as an expected cosmetic cycle. A naive wait would turn a known blip into a red CI job.

Rules:

- Failure is `deploy_failed` or `build_failed` **sustained across N consecutive polls** with no in-flight build, not first observation.
- Per-target default timeouts. `--for destroyed` defaults to 15+ minutes: destroy convergence is owned by a 2-minute cron with an 8-minute escalation window, so legitimate destroys routinely exceed 10 minutes.
- Terminal and failure state sets are derived from the machine, never hand-listed — the repo already enforces this convention for `TERMINAL_STATES` and friends.

### Attribution

Audit rows must distinguish CLI actions from web actions, recording actor email, channel, credential name and client version. `insertAuditLog` (`src/lib/deps.ts:148-160`) has no such columns today. Given that the two forks' migration chains collide by number, v1 carries these in the existing `details` JSONB rather than adding columns; promote to columns later if query patterns demand it.

### Testing

- Unit tests per command with golden output files for table and JSON rendering.
- Contract conformance: generated client compiles against each vendored spec; each server spec is asserted a superset of the pinned contract.
- End-to-end smoke against an ephemeral environment in CI, gated behind a real credential.
- Server side follows existing repo convention — every procedure has a colocated test driving the extracted core.

### Observability

`/api/v1` request logs carry credential id, procedure name, and outcome; 4xx and 5xx rates are metricated per procedure. None of this exists for the current routes and it is the only way to see CLI adoption or abuse.

## 6. Distribution

Source and releases live in a neutral Steadfast organisation (`~/sf/drift-cli`), so neither fork references the other's lineage. goreleaser publishes multi-arch binaries to GitHub Releases; installation via `mise use -g github:...` or a curl installer. Semver, independent of the servers' `stg-<sha>` / `rc-<date>` image tagging — compatibility is governed by the discovery document, not lockstep versions.

## 7. Sequencing

Revised after review. The governing principle: a working command exists early, and the auth migration is never on the critical path.

0. **Spikes**, in a worktree, AU only. (a) oRPC: 2-3 endpoints, emit the spec, run `oapi-codegen` against it, validate the generated Go client and the error envelope. (b) Better Auth: pin a version, run `generate`, wire the Drizzle adapter, close the four unverified hook questions. Half a day each; go/no-go on both, independently.
1. **Server hardening** — JSON 401/403, read-role floor on unguarded GETs. Ships on its own merits.
2. **Action-layer normalization** — hoist `requireRole` out of all 17 actions, introduce the typed error union, lift inline schemas into `src/schemas/`. Independently valuable; unblocks everything downstream.
3. **Vertical slice** — oRPC for reads, `/.well-known/drift.json`, web-minted scoped credential under existing auth, and Go CLI v0.1 (`auth login`, `context`, `env list/get`, `doctor`, `version`, JSON output). This proves spec to codegen to cobra to auth to RBAC end-to-end without touching web authentication.
4. **Mutations** — full `/api/v1`, `.actionable()` cutover action-by-action rather than big-bang, CLI environment lifecycle, wait semantics, audit attribution.
5. **Rate limiting and promotions** — per-credential limits on mutations, `promote rc` and `hotfix`.
6. **prd promotion** behind the scoped short-TTL re-login mint.
7. **Better Auth migration**, as its own project: schema land, cutover deploy, device flow. CLI login upgrades from paste to device flow; the credential table maps onto `@better-auth/api-key` or stays as-is.
8. **EN port.** Scoped honestly as a re-implementation, not a port: EN has no `auth.config.ts` and its `config.ts` predates the entire `AUTH_PROVIDER` refactor. Step zero for EN is verifying the Entra email claim, which is the one thing that can hard-fail there.

## 8. Open risks

- oRPC v2 beta releases daily and the project changed organisations this year; pinned to 1.14.x. Emitted-spec quality under Go codegen is unproven — hence the spike gate.
- `oapi-codegen` OpenAPI 3.1 support is initial (v2.8.0, 2026-07-17). Keep 3.1-to-3.0 downconversion as an escape hatch.
- Action-layer normalization touches all 17 actions and every page that wires them. It is the largest single refactor in the plan and the most likely place to stall.
- Better Auth ships breaking minors on a weeks-long cadence and 1.7 GA is an external date outside our control. Under the revised sequencing a slip delays only step 7, with the CLI already shipped.
- Better Auth cutover kills every live session — cookie names and session strategy both change. One-time forced global re-login, schedulable, low blast radius for a VPN-gated internal tool.
- Auth will touch Postgres on every non-cookie-cached `getSession`; today's JWT path never does. Size the connection pool deliberately.
- AU and EN diverging on the shared surface is the standing threat to "one binary". The vendored-spec conformance test is the only durable guard.
- The two forks' migration chains collide by number; SQL cannot be copied between repos without renumbering.
