# Support

## Supported scope

Support covers the latest v0.7 maintenance release with Go 1.22 or newer. The
interactive transport is supported only with Codex CLI 0.153.1 and 0.153.4 and
an intact reviewed configuration contract.

Use GitHub issues for reproducible bugs and focused maintenance requests.
Include the codex-go revision, Codex CLI version, operating system, execution
policy, transport (`Run` or `RunInteractive`), and a minimal reproduction.
Remove credentials, prompts containing private data, MCP environment values,
and sensitive app-server output.

## Out of scope

Maintainers cannot provide:

- support for unreviewed Codex CLI versions or current-main APIs;
- deployment-specific authentication or authorization design;
- approval of plugins, MCP servers, account access, or YOLO operation;
- recovery guarantees for commands or external effects;
- help bypassing reviewed-configuration failures.

Security concerns must follow [SECURITY.md](SECURITY.md), not a public support
issue.
