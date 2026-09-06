# codex

A Go client for running the official [OpenAI Codex CLI](https://github.com/openai/codex) as a subprocess. It starts or resumes sessions, parses JSON events, and returns the last completed agent message and session ID.

[agent-go](https://github.com/teslashibe/agent-go) uses this package as its Codex execution adapter. The package can also be used independently; it does not implement the agent-go application, a model API client, or a standalone command.

## Requirements and installation

- Go 1.22 or newer, as declared in `go.mod`.
- An installed, authenticated Codex CLI for real invocations. Authentication and session storage remain managed by the CLI through `CODEX_HOME`.
- A CLI version supporting the flags used in `codex.go`, including `--ignore-user-config` and `--ignore-rules`. Source comments document policy/output-schema behavior against Codex 0.153.1 and 0.153.4; other versions require compatibility verification.

With repository access and Git authentication configured, add the module from your application's Go module:

```sh
GOPRIVATE=github.com/teslashibe/codex go get github.com/teslashibe/codex
```

The repository is private; the module path does not imply anonymous access or a published release. If you already configure `GOPRIVATE`, include this module alongside your existing patterns.

## Usage

This example invokes Codex, but does not register MCP servers or enable command writes:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/teslashibe/codex"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	client := &codex.Client{WorkDir: "."}
	result, err := client.Run(ctx, "", "Summarize the repository without changing files.")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(result.Text)
	// Retain result.SessionID and pass it to Run to resume the session.
}
```

An empty session ID starts a session; resuming requires a UUID. `Client` defaults to `codex` on `PATH`, the current directory, the CLI's default model, a five-minute timeout, and read-only execution. Do not modify a client while `Run` is executing.

Optional fields configure the binary, working directory, model, timeout, developer instructions, JSON output schema, reasoning effort, service tier, and trusted stdio MCP servers. `OutputSchema` must be a JSON object; Codex validates its semantics, and the response remains a string in `Result.Text`.

## Execution and safety

- `ExecutionReadOnly` is the default for new and resumed sessions.
- `ExecutionWorkspaceWrite` permits writes within the CLI's workspace sandbox.
- `ExecutionYOLO` explicitly selects native `--yolo`, removing command sandboxing and approval prompts. OS permissions and installed tool/account access still apply.

The caller must authenticate and authorize the user before selecting an execution policy. Never derive policy from model output or tool content. Execution is noninteractive: the normal policies use `approval_policy="never"`, so requests requiring escalation fail rather than opening an approval channel.

User configuration and execpolicy rules are ignored. Project instructions and credentials are not isolated. Only explicitly configured `MCPServers` are enabled, and **MCP servers run outside the command sandbox**: even read-only execution can cause external mutations through a trusted tool. Enforce tool authorization separately. MCP configuration is passed in command arguments; treat commands and environment values as sensitive.

Prompts, instructions, schemas, and combined MCP overrides are each bounded to 1 MiB; stdout is bounded to 16 MiB and stderr to 64 KiB. Cancellation kills the subprocess group on macOS/Linux and only the CLI process on other platforms, with bounded pipe cleanup. Platform support for actual execution also depends on the installed Codex CLI.

Success requires a started thread, a completed turn, and successful process exit. On error, `Result.Text` is empty and the session ID is retained when known. Cancellation or failure does not roll back commands or MCP effects: reconcile uncertain effects before retrying.

## Development

From the repository root:

```sh
go build ./...
go vet ./...
go test ./...
```

Tests are intended to use fake subprocesses rather than an authenticated Codex session. At the current source revision, `go vet ./...` and `go test ./...` are blocked by an undefined `runRPCFixture` reference in `codex_test.go`; `go build ./...` succeeds. There is no executable in this module to install with `go install`.

## License

[MIT](LICENSE). The external Codex CLI is distributed separately under its own license.
