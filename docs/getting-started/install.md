# Install

## mise (recommended)

[mise](https://mise.jdx.dev/) installs the latest release and keeps it
up to date:

```bash
mise use -g github:steadfast-ly/drift-cli
```

## go install

If you have a Go toolchain:

```bash
go install github.com/steadfast-ly/drift-cli@latest
```

The binary is named `drift`.

## GitHub Releases

Download a pre-built binary from
[GitHub Releases](https://github.com/steadfast-ly/drift-cli/releases).
Archives are published for every tagged version:

| OS | Architecture | Archive |
| -- | ------------ | ------- |
| Linux | amd64 | `drift_<version>_linux_amd64.tar.gz` |
| Linux | arm64 | `drift_<version>_linux_arm64.tar.gz` |
| macOS | amd64 | `drift_<version>_darwin_amd64.tar.gz` |
| macOS | arm64 | `drift_<version>_darwin_arm64.tar.gz` |
| Windows | amd64 | `drift_<version>_windows_amd64.zip` |
| Windows | arm64 | `drift_<version>_windows_arm64.zip` |

Each release includes a `checksums.txt` for verification. Extract the archive
and place the `drift` binary on your `PATH`.

## Shell completion

Generate a completion script for your shell:

```bash
# bash
drift completion bash > /etc/bash_completion.d/drift
# or per-user:
drift completion bash > ~/.local/share/bash-completion/completions/drift

# zsh
drift completion zsh > "${fpath[1]}/_drift"

# fish
drift completion fish > ~/.config/fish/completions/drift.fish
```

## Version compatibility

Client and server versions are independent. Compatibility is governed by the
server's discovery document (`/.well-known/drift.json`), not by matching
version numbers. A version difference between the CLI and the server is
expected and normal. The server advertises a minimum client version; when the
CLI is older it warns loudly but never refuses to operate.
