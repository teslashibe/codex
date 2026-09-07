# Contributing

The v0.7 branch is a compatibility maintenance line for consumers that depend
on its interactive app-server APIs. Keep changes small and preserve existing
behavior.

## Before proposing a change

1. Start from the v0.7 maintenance branch, not current `main`.
2. Open an issue for protocol, configuration-contract, execution-policy, or
   public-API changes before implementation.
3. Do not add support for a Codex CLI version without reviewing its app-server
   protocol, configuration sources and merge behavior, effective sandbox and
   approval responses, and adding representative tests.
4. Do not weaken fail-closed validation to accommodate a deployment.

## Development checks

Use a supported Go toolchain and Python 3.11 or newer for the TOML
compatibility test, then run:

```sh
go mod tidy
git diff --exit-code -- go.mod go.sum
go build ./...
go vet ./...
go test -race ./...
govulncheck ./...
```

Changes should include focused tests where behavior changes. Keep generated
files, credentials, local Codex homes, and worktrees out of commits. Document
security and compatibility consequences in the pull request.

By contributing, you agree that your contribution is licensed under the
repository's MIT License.
