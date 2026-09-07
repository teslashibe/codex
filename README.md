# codex-go v0.7

`github.com/teslashibe/codex` is a small Go client for the official Codex CLI.
The v0.7 line preserves the interactive and app-server APIs used by
`agent-go`; these APIs are intentionally maintained separately from newer
branches where they no longer exist.

## Compatibility and maintenance

- Go 1.22 or newer.
- Codex CLI **0.153.1** and **0.153.4** are the only reviewed versions for
  `RunInteractive`.
- The noninteractive `Run` transport also targets the v0.7-era CLI protocol.
- v0.7 receives focused compatibility, security, and correctness maintenance.
  It is not the development line for new Codex features.
- Released maintenance versions are identified by immutable v0.7.x tags.

Codex app-server is not a stable public protocol. A different CLI version must
be reviewed against its wire messages, effective sandbox and approval fields,
configuration loading, plugin merge behavior, and source discovery before it
is added to the allowlist.

## APIs

`Client.Run` invokes `codex exec --json`. It starts or resumes a session and
returns the last completed agent message. It is noninteractive: requests that
need escalation fail. `ExecutionAccountAccess` is therefore rejected.

`Client.RunInteractive` invokes `codex app-server --stdio`. It supports:

- starting and resuming threads;
- starting one turn and returning its last completed agent message;
- command, file-change, and narrowly supported MCP elicitation approvals;
- per-request `ApprovalDecision` values (`ApprovalDeny`, `ApprovalOnce`, and
  `ApprovalCancel`);
- reviewed static MCP servers and reviewed per-run MCP bindings;
- read-only, workspace-write, account-access, and explicit YOLO policies.

The callback is not an authorization boundary by itself. The caller must
authenticate and authorize the human before returning an approval. A nil
handler denies approval requests, and account access requires a handler.
App-server may execute sandbox-allowed actions without emitting an approval.

## Reviewed interactive configuration

Every interactive run requires `Client.InteractiveConfig` to contain a trusted
`ReviewedInteractiveConfig`. It is deployment review data, never model, chat,
or untrusted configuration input.

The contract pins:

- absolute, clean Codex home, reviewed workspace, and Codex binary paths;
- the exact supported CLI version and the binary's `--version` output;
- the SHA-256 of `config.toml`;
- the deterministic SHA-256 of explicit static MCP definitions;
- every known configuration, policy, rules, hooks, and instruction source,
  including expected-absent paths;
- every reviewed plugin as explicitly enabled or disabled;
- optional per-run MCP bindings, including their fixed command, arguments,
  environment and exact dynamic environment-key set.

Use `InteractiveConfigSources` to enumerate required source paths and
`MCPServersSHA256` to fingerprint static MCP definitions. Review the actual
bytes before recording hashes. Empty source digests mean the path must not
exist. Do not include credentials in the source map.

Validation fails closed on drift, unexpected sources, routing environment
variables, unreviewed plugins or bindings, unsupported versions, and changed
MCP definitions. It rechecks sources immediately before app-server launch.
This is a drift check, not protection from concurrent changes by the same OS
user; keep the reviewed configuration stable for the duration of a run.

## Minimal usage

```go
client := codex.Client{
    Binary:          "/absolute/path/to/codex",
    WorkDir:         "/absolute/path/to/workspace",
    ExecutionPolicy: codex.ExecutionReadOnly,
    InteractiveConfig: reviewedConfig, // constructed from an out-of-band review
}

result, err := client.RunInteractive(ctx, "", "Inspect the repository.", func(
    ctx context.Context,
    request codex.ApprovalRequest,
) (codex.ApprovalDecision, error) {
    return codex.ApprovalDeny, nil
})
```

Set `CODEX_HOME` to the exact absolute `ReviewedInteractiveConfig.CodexHome`
before calling `RunInteractive`. To use a reviewed dynamic MCP binding, pass an
`MCPBinding` whose name and complete environment map exactly match the pinned
binding definition.

## Risks and operational requirements

- `ExecutionAccountAccess` and `ExecutionYOLO` remove the Codex command
  sandbox. They expose everything available to the process's OS account.
- MCP servers run outside the command sandbox and can mutate external systems
  even when the execution policy is read-only.
- Approval request fields are untrusted display data, not proof of identity or
  authorization. `ApprovalOnce` applies only to that request.
- Cancellation, denial, transport errors, and failed turns do not roll back
  earlier command or MCP effects. `InteractiveError.MayHaveSideEffects` means
  the session must be reconciled before retrying.
- Session IDs, configuration sources, plugin state, protocol fields, and
  effective sandbox settings are checked strictly. Do not bypass failures by
  weakening the review or silently selecting another home or binary.
- Command arguments may contain MCP configuration. Treat process inspection
  access as sensitive and avoid placing secrets in fixed definitions where a
  reviewed per-run binding is appropriate.

See [SECURITY.md](SECURITY.md), [CONTRIBUTING.md](CONTRIBUTING.md), and
[SUPPORT.md](SUPPORT.md) before deploying or proposing changes.
