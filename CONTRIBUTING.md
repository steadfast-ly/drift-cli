# Contributing

External contributions are not accepted. Issues and Discussions are disabled on
this repository.

The source is published so that client security teams can review what runs on
their engineers' laptops. The decision, its rationale and the trade-offs are
recorded in the server repository's ADR-0015 ("The CLI stays a separate, public
repository under Apache-2.0").

If you believe you have found a security vulnerability, contact Steadfast
through your engagement.

## Spec and generated client

A spec change (`spec/openapi.json`) and its regenerated client
(`internal/api/client.gen.go`) must land in the same commit. CI enforces this
with `make check-generated` -- a commit that carries one without the other
fails the build.

For server releases, `.github/workflows/spec-sync.yaml` automates the update:
the server pushes the new spec to a `spec-sync/` branch, the workflow
regenerates the client into the same commit, and opens a CI-checked PR.

For manual updates, `make vendor-spec SERVER_REPO=/path/to/drift/checkout`
revendors and regenerates in one step.

## Filing issues

Findings and improvement ideas should be raised with the maintainer for triage
before filing as issues. Do not file issues without prior approval.

Issues must never contain customer-deployment-specific data -- hostnames, org
names, account identifiers, or any other detail that identifies a particular
deployment. This is a product repository; deployment details are private.
Use generic placeholders when an example is needed.
