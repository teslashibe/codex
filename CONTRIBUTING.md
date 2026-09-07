# Contributing

Contributions are welcome through GitHub issues and pull requests.

Before proposing a change:

1. Check existing issues and pull requests.
2. Keep the change focused and preserve the package's noninteractive API.
3. Add or update tests for behavior changes.
4. Do not include credentials, private configuration, or real session data.

Run the same checks used by CI:

```sh
go mod tidy
git diff --exit-code -- go.mod go.sum
go build ./...
go vet ./...
go test ./...
go test -race ./...
```

Tests must use fake subprocesses and must not require an authenticated Codex session. If a change depends on Codex CLI behavior, state the CLI version used for verification and update the compatibility documentation when appropriate.

By contributing, you agree that your contribution is licensed under the repository's MIT License.
