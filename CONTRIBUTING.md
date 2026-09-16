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
with `just check-generated` -- a commit that carries one without the other
fails the build.

For server releases, `.github/workflows/spec-sync.yaml` automates the update:
the server pushes the new spec to a `spec-sync/` branch, the workflow
regenerates the client into the same commit, and opens a CI-checked PR.

For manual updates, `just vendor-spec /path/to/drift/checkout`
revendors and regenerates in one step.

## Releases

`just release X.Y.Z` is the sanctioned release procedure: it fetches,
refuses a version that already exists and a tag target whose commit message
carries a GitHub skip marker (those markers suppress tag-push workflows and
would burn the tag silently), tags origin/main's HEAD with an annotated
`vX.Y.Z` and pushes it. `.github/workflows/release.yaml` builds the versioned
binaries from the tag, reusing the green push-to-main CI run for that exact
commit when one exists and falling back to the full `just check` gate when it
does not. Nothing releases on a merge to main, and tags are immutable: repair
a bad release by cutting the next version, never by re-tagging. Merge any
pending spec-sync PR first when the release should carry a new server
contract.

## Filing issues

Findings and improvement ideas should be raised with the maintainer for triage
before filing as issues. Do not file issues without prior approval.

Issues must never contain customer-deployment-specific data -- hostnames, org
names, account identifiers, or any other detail that identifies a particular
deployment. This is a product repository; deployment details are private.
Use generic placeholders when an example is needed.
