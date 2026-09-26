# Promotions

## Everyday workflow

### Check release state

```bash
drift release status
drift release history
```

`release status` shows what is deployed to stg and rc, with pod health.
`release history` lists past promotions.

### Promote to rc

```bash
drift release promote rc svc-a svc-b --yes
```

This retags each service's current stg image as rc and dispatches the
workflow. Blocks until the promotion finishes deploying (20-minute timeout).

### Hotfix to rc

```bash
drift release promote hotfix svc-a --branch hotfix/urgent --yes
```

Builds a branch straight to rc, bypassing stg. For emergencies only --
nothing has validated the build before it reaches rc.

## Production promotion

Production promotion is the highest-consequence operation in drift. It
requires a short-lived elevated credential that cannot be minted
programmatically.

### Step 1: Mint an elevated credential

In the web UI at `<endpoint>/credentials`, the **"Elevate for production"**
card is visible to users with `release` role and above. It mints a
**15-minute** token scoped to `promote:prd`.

- The only input is a **label**.
- It requires an interactive browser sign-in (SSO).
- It cannot be minted via the API, renewed, or extended.
- It cannot be minted by a pipeline -- a token can never mint another token.

### Step 2: Run the promotion

The recommended approach is a **one-shot environment variable**, which never
touches your stored long-lived credential:

```bash
DRIFT_TOKEN=drift_xxx drift release promote prd svc-a --yes
```

!!! tip "Keep the token out of shell history"

    The inline form above puts a production-capable token in your shell
    history. This history-safe pattern avoids that; the 15-minute expiry
    limits but does not eliminate exposure.

    ```bash
    read -rsp 'Elevated token: ' DRIFT_TOKEN; echo
    DRIFT_TOKEN="$DRIFT_TOKEN" drift release promote prd svc-a --yes
    unset DRIFT_TOKEN
    ```

!!! warning "Do not use `drift auth login` with an elevated token"

    Piping the elevated token into `drift auth login --token-stdin` works,
    but it **overwrites** your stored long-lived credential -- when the
    elevated token expires 15 minutes later you are fully logged out. The
    `DRIFT_TOKEN` one-shot leaves your stored credential untouched. (Older
    server versions may still suggest the login path in their error hint.)

### Failure signature

A promotion without the `promote:prd` scope fails with:

- HTTP 403 with problem type `urn:drift:problem:elevation-required`
- CLI exit code **4** (authentication required)

```
Error: Elevated credential required
  This operation requires a credential scoped to promote:prd.

Hint: mint a 15-minute elevated credential at /credentials, then retry
      with DRIFT_TOKEN=<credential> set for that one command
```

### Production hotfix

```bash
DRIFT_TOKEN=drift_xxx drift release promote prd hotfix svc-a \
  --branch hotfix/critical --yes
```

Builds a branch straight to production, bypassing both stg and rc. Requires
the same elevated credential.

## Concurrency

The concurrency guard is service-scoped. Disjoint services promote in
parallel; overlapping services (same service, same repo, or shared application
group) get a **409 conflict**.

A promotion whose target already runs the requested tag healthily completes
without dispatching a new workflow.

## Blocking

All promotions block by default (20-minute timeout). Use `--no-wait` to
return as soon as the workflows are dispatched, then check progress with
`drift release status` or `drift release history`.

## Cancelling a stuck promotion

A promotion whose retag workflow was cancelled, whose target was rolled back
by hand, or that never received its ArgoCD notification stays in flight
forever. While it does, the concurrency guard refuses the next promotion of
the same services, and no `--wait` will ever finish.

`drift release cancel` fails it:

```bash
drift release cancel <promotion-id> --reason "retag workflow was cancelled"
```

| In-flight state | Resulting state |
| --------------- | --------------- |
| `dispatched`    | `failed`        |
| `promoting`     | `failed`        |
| `deploying`     | `deploy_failed` |

There is no separate `cancelled` state, and a cancelled promotion cannot be
resumed -- promote the services again instead. The `--reason` is optional
(1-500 characters after trimming, counting an emoji as two); when given it is
recorded on the promotion and in its `promotion.canceled` audit row.

The id comes from `drift release status` (the in-flight promotion) or
`drift release history -o wide` (the `id` column is wide-only). Cancelling
needs the **release** role, and the server must advertise the
`promotions.cancel` capability.

Cancelling is destructive, so it confirms on a terminal and takes `--yes`;
a non-interactive session without `--yes` refuses with exit **2**, sending no
cancel request and making no promotion lookup either -- the refusal is
decided locally, so it cannot be turned into a connection error by an
unreachable server. The promotion id is validated client-side first, so a
mistyped id is also exit **2** rather than a round trip. Failures follow the
standard codes: an unknown id is exit **3** (not found), and a promotion that
has already finished is exit **5** (state conflict, naming the state it is
in).
