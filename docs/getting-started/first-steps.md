# First steps

This page walks through connecting the CLI to a drift deployment for the
first time.

## 1. Add a context

A context is a named endpoint. The endpoint is the deployment's **origin**
(e.g. `https://drift.example.com`), never an API path -- the API base is
discovered automatically from `/.well-known/drift.json`.

```bash
drift context add prod --endpoint https://drift.example.com
```

The first context you add becomes the current context automatically.

## 2. Mint a credential

Open the credentials page in your browser:

```
https://drift.example.com/credentials
```

Sign in through your organisation's SSO, then use the **CLI Credentials**
section to mint a token:

- **Label** -- a short name so you can tell credentials apart later
  (e.g. "laptop", "CI runner").
- **Role** -- the authority level. You can mint at or below your own role.
- **TTL** -- how long the token lives. Presets are 24 hours, 7 days
  (default) and 30 days. The hard cap is 30 days.

The token looks like `drift_...` and is shown **once**. Copy it immediately
-- it cannot be recovered. The label is how you identify the credential
afterwards.

## 3. Log in

```bash
drift auth login
```

The CLI prints the credentials page URL, then prompts for the token. Paste it
(the input is hidden). The CLI validates the token against the server before
storing it -- a mistyped or already-revoked token fails here rather than on the
next command.

For non-interactive use (e.g. piping from a secret manager):

```bash
echo "drift_..." | drift auth login --token-stdin
```

### Where credentials are stored

The CLI stores credentials in the **OS keyring** where one is available, and
falls back to a `credentials.yaml` file with mode 0600 in the config
directory. `drift auth login` reports which store was used.

The config file (`config.yaml`) holds **no secrets** and is safe to commit to
a dotfiles repository.

## 4. Verify

```bash
drift auth status
```

Shows the active context, the credential source, the identity (email), role,
scopes and expiry. A warning appears when the credential expires within
24 hours.

```bash
drift doctor
```

Runs all checks end to end: config, network, discovery, auth and version
skew.

## 5. Start using drift

```bash
# List environments.
drift env list

# Create one from the current branch (inside a git checkout).
drift env create

# Check release state.
drift release status
```
