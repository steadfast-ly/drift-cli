# Updating drift

`drift` can update itself from the latest
[GitHub release](https://github.com/steadfast-ly/drift-cli/releases).

## Update immediately

```bash
drift self-update
```

This downloads the archive for your platform, verifies its SHA256 against the
release's `checksums.txt`, and atomically replaces the running binary. It
prints progress to stderr:

```
Updated drift from 0.14.2 to 0.15.0
```

If you are already current:

```
Already up to date (0.14.2)
```

### Query without installing

For scripting, `--check` reports whether an update is available without
downloading anything, and exits 0 in both cases:

```bash
drift self-update --check
drift self-update --check --json current_version,latest_version,update_available
```

Exit 0 means the *query* succeeded; the `update_available` field tells you the
answer. Exit 1 means the check failed (network, GitHub API, parse error).

## Passive weekly nudge

The CLI checks GitHub for a newer release at most once per week and prints a
one-line nudge to stderr after a command completes when one exists:

```
drift v0.15.0 is available (you have v0.14.2); run 'drift self-update'.
```

The nudge never touches stdout, so `drift env list -o json > file.json` stays
parseable. A failed check is silent — no nudge, no error — and the check is
not retried for another week, so offline users are not stalled on every
command.

The result of each check is cached in `update-check.json` in the config
directory. If you swap the binary manually, the cache notices the version
change and checks again immediately.

## Opting out

Set `DRIFT_NO_UPDATE_CHECK=1` to disable the nudge entirely:

```bash
export DRIFT_NO_UPDATE_CHECK=1
```

The nudge is also disabled automatically whenever `CI` is set (any value), for
`drift self-update`, `drift version`, and `drift completion`, and on dev
builds.

## Managed installs

If drift was installed by a package manager, `self-update` refuses to touch it
and tells you which command to use instead:

- **mise**: `mise upgrade drift` (or `mise install drift`)
- **Homebrew**: `brew upgrade drift-cli`

## Dev builds

If your binary is a dev build (the version is not a plain `X.Y.Z` release,
e.g. `0.1.0-dev` or a git describe string), `self-update` is not available —
build from source or install a release:

```
Error: self-update is not available for dev builds (version 0.1.0-dev); build from source or install a release
```

`--check` still works on dev builds: it is a read-only query and only the
actual update is restricted to release builds.
