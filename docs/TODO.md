# drift-cli — TODO

Deferred items, in priority order. Each records what is wrong, how it was found,
and why it was not fixed at the time.

## 1. `auth status` claims `/api/v1` does not expose the credential's owner, role and expiry — it does

`auth status` prints:

> The credential's owner, role and expiry are not exposed by /api/v1; see
> `<endpoint>/credentials`.

That is false. `GET /api/v1/auth/whoami` returns all three. Verified live against
the `sf-drift` install, 2026-08-25:

```json
{"email":"felipe.scarel@auditsight.com","role":"admin","channel":"cli",
 "credential":{"id":"…","label":"desktop","scopes":[],
               "expiresAt":"2026-09-24T19:19:59.694Z"}}
```

The expiry is the reason `auth.whoami` exists at all: without it a CLI discovers a
dead credential by being refused, and `doctor` can only warn after the fact. The
CLI is currently sending the operator to a browser for something it could print.

**Fix:** call `auth/whoami` from `auth status` and render owner, role, channel,
label, scopes and expiry. Drop the disclaimer. Consider warning when the expiry is
inside some window.

## 2. `doctor` reports a server version that is not the deployment's version

`doctor` and `auth status` both show `Server version: 1.0.0` against a deployment
running release **0.1.1**. Confirmed live, same session.

The fault is server-side, not in the CLI: `DRIFT_VERSION` in the Drift repo's
`src/lib/discovery.ts` is a hardcoded `"1.0.0"`, pinned by a test to
`package.json`'s version — and `package.json` is never stamped at release.
`scripts/rollout/stamp-chart.sh` stamps the CHART's version, appVersion and the
image tag, which ADR-0008 requires to be one number. Discovery is not in that
chain and should be.

It is benign today because skew is advisory — `version skew ok, client 0.1.0-dev
meets the minimum 0.1.0` — but the version-skew check is the one thing in `doctor`
reporting a number that is not real, and it will mislead exactly when someone is
debugging a mismatch.

**Fix (server-side, in the Drift repo):** make the release stamp reach discovery —
either stamp `package.json` alongside the chart, or have the chart inject the
release semver as an environment variable that `discovery.ts` reads, with the
existing agreement test extended to cover it. Then `minimum_client_version` can be
reasoned about against a real number.

**Found by:** standing up the `sf-drift` proof install at AuditSight and pointing
the CLI at it over the internal ingress. Neither surfaces locally: discovery
returns the same constant either way, and nothing else compares it to the deployed
artifact's tag.
