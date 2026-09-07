# Security policy

## Supported versions

The latest released v0.7 patch is the supported maintenance line. Interactive
use is supported only with the reviewed Codex CLI versions listed in the
README. Older patches and unreviewed CLI versions may not receive fixes.

## Reporting a vulnerability

Do not open a public issue for a suspected vulnerability. Use GitHub's private
security-advisory reporting for this repository and include:

- the affected codex-go and Codex CLI versions;
- the execution policy and whether `Run` or `RunInteractive` was used;
- a minimal reproduction with secrets and personal data removed;
- the observed security impact and any known mitigations.

Please allow maintainers time to reproduce and coordinate a fix before public
disclosure. If private reporting is unavailable, contact a repository
maintainer privately without including secrets in the first message.

## Security boundaries

The Go client does not authenticate users, isolate OS accounts, or make MCP
servers safe. Callers own authentication, authorization, trusted configuration,
approval UI, and reconciliation of uncertain external effects. Account-access
and YOLO policies remove the Codex command sandbox; MCP servers execute outside
that sandbox under every policy.

The reviewed interactive configuration is a fail-closed drift contract, not a
defense against concurrent modification by the same OS user. Do not work around
its validation failures or use an unreviewed Codex CLI release.
