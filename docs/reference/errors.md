# Errors and exit codes

## Exit codes

| Code | Meaning |
| ---- | ------- |
| 0 | Success |
| 1 | Error (generic failure) |
| 2 | Usage -- bad flags or arguments |
| 3 | Not found |
| 4 | Authentication required |
| 5 | State conflict or operation failed |
| 6 | Wait timed out |
| 7 | Rate limited |

Exit codes are a frozen public contract.

## API problem envelope

Non-success responses from the drift API carry a structured problem envelope:

```json
{
  "message": "Cannot sleep environment",
  "code": "CONFLICT",
  "status": 409,
  "defined": true,
  "data": {
    "type": "urn:drift:problem:invalid-transition",
    "detail": "the environment is building, which does not accept SLEEP",
    "state": "building",
    "event": "SLEEP"
  }
}
```

The `data.type` field is a URN that identifies the error class. The CLI maps
each URN to the appropriate exit code and hint.

## URN registry

| URN | HTTP status | Exit code | What to do |
| --- | ----------- | --------- | ---------- |
| `urn:drift:problem:unauthenticated` | 401 | 4 | Mint a fresh credential and run `drift auth login`. The server deliberately gives one message for expired, revoked and unknown tokens. |
| `urn:drift:problem:forbidden` | 403 | 1 | Your role does not permit this operation. Re-authenticating will not help. |
| `urn:drift:problem:elevation-required` | 403 | 4 | The operation requires a `promote:prd` scope. Mint a 15-minute elevated credential at `/credentials` and retry with `DRIFT_TOKEN=<credential>`. See [Promotions](../guides/promotions.md). |
| `urn:drift:problem:not-found` | 404 | 3 | The resource does not exist. For slugs: a slug resolves only live environments; use the UUID for destroyed or canceled ones. |
| `urn:drift:problem:validation` | 400 | 2 | A request field failed validation. Check the `data.detail` message. |
| `urn:drift:problem:invalid-transition` | 409 | 5 | The entity is in a state that does not accept the requested event. The `data.state` and `data.event` fields describe the conflict. The same retry will fail the same way -- the state must change first. |
| `urn:drift:problem:conflict` | 409 | 5 | A concurrency conflict -- you lost a race. A retry may win. |
| `urn:drift:problem:external-service` | 502 | 1 | A typed error from the forge (e.g. GitHub). Commonly seen with `repo branches` when forge credentials are misconfigured. |
| `urn:drift:problem:rate-limited` | 429 | 7 | Per-credential rate limit. The `Retry-After` header says how long to wait. |
| `urn:drift:problem:service-unavailable` | 503 | 1 | The server is temporarily unavailable. Retry after a short wait. |
| `urn:drift:problem:internal-error` | 500 | 1 | An unexpected server error. Report it if it persists. |

## Rate limiting

The API limits per credential with two buckets (defaults: 120 reads + 20
mutations per 60 seconds). A 429 response carries `Retry-After` in whole
seconds.

- The CLI **never retries a mutation** automatically.
- A `--wait` poll loop backs off by the server's own number and carries on;
  the wait deadline still applies, so a persistently throttled wait ends as a
  timeout (exit 6).
- A `Retry-After` in the RFC 9110 HTTP-date form (from an ALB, WAF or CDN)
  is converted to seconds.
- A 429 that does not arrive as a drift envelope stays on the generic error
  path (exit 1) -- an intermediary shedding load is not drift rate-limiting
  the caller.

## Redirects

The CLI refuses to follow redirects by design. A redirect from the configured
endpoint could send credentialled requests to an unintended destination.
