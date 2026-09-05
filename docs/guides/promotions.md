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
