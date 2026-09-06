// Package codex runs the official Codex CLI with an explicit execution policy.
// The default policy is read-only; sandbox access is not interactive approval.
package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxPrompt = 1 << 20
	maxSchema = 1 << 20
	maxStdout = 16 << 20
	maxStderr = 64 << 10
	waitDelay = time.Second
)

// ExecutionPolicy selects filesystem/network sandbox access, not who may ask
// for it or whether individual actions are approved. Callers must authenticate
// and authorize the sender and chat before selecting a policy. Never derive it
// from model output, retrieved content, or an unauthenticated text request.
type ExecutionPolicy string

const (
	ExecutionReadOnly       ExecutionPolicy = "read-only"
	ExecutionWorkspaceWrite ExecutionPolicy = "workspace-write"
	// ExecutionAccountAccess removes the Codex command sandbox. It does not
	// elevate the OS user or grant root, credentials, or macOS privacy access.
	// It is not account isolation: commands can access everything available to
	// the process's OS account. Run rejects it until interactive approval is
	// supported; combining it with exec's never policy would bypass approvals.
	ExecutionAccountAccess ExecutionPolicy = "account-access"
)

// SandboxMode validates p and returns the documented Codex sandbox mode used by
// Codex 0.153.1 and 0.153.4. Empty preserves the read-only default. This mapping
// alone does not authorize execution or establish an approval channel.
func (p ExecutionPolicy) SandboxMode() (string, error) {
	switch p {
	case "", ExecutionReadOnly:
		return "read-only", nil
	case ExecutionWorkspaceWrite:
		return "workspace-write", nil
	case ExecutionAccountAccess:
		return "danger-full-access", nil
	default:
		return "", errors.New("codex: invalid execution policy: want read-only, workspace-write, account-access, or empty")
	}
}

// ErrInteractiveApprovalRequired means the requested execution policy cannot be
// used with the noninteractive exec transport. Do not retry with weaker guards.
var ErrInteractiveApprovalRequired = errors.New("codex: account-access requires an interactive approval transport; exec is unsupported")

// Client configures Codex invocations. Its zero value uses codex from PATH,
// the current directory, the CLI's default model, and a five-minute timeout.
// Do not modify a Client while Run is executing.
// Auth and session storage remain managed by the CLI through CODEX_HOME.
// Run limits prompts, instructions, and output schemas to 1 MiB each,
// stdout to 16 MiB, and stderr to 64 KiB.
// Cancellation kills the process group on macOS/Linux; elsewhere only the CLI
// process is killed. Pipe cleanup is bounded to one additional second.
type Client struct {
	Binary  string
	WorkDir string
	Model   string
	Timeout time.Duration

	// ExecutionPolicy defaults to read-only for new and resumed sessions.
	// Workspace-write permits unattended writes within the CLI's sandbox;
	// commands requiring escalation still fail, rather than prompt. Account
	// access is validated but Run rejects it before starting the CLI.
	// This does not restrict MCP tools: trusted MCP servers run outside the
	// sandbox and may mutate external systems even under read-only policy.
	ExecutionPolicy ExecutionPolicy

	// Instructions overrides developer_instructions for new and resumed sessions.
	// Empty keeps the CLI default; values are limited to 1 MiB.
	Instructions string

	// OutputSchema optionally constrains the final response for new and resumed
	// sessions. It must be a JSON object of at most 1 MiB; empty disables it.
	// The CLI validates schema semantics. The final JSON remains in Result.Text.
	OutputSchema json.RawMessage

	// ReasoningEffort overrides model_reasoning_effort. Allowed values are
	// none, minimal, low, medium, high, and xhigh; empty keeps the CLI default.
	ReasoningEffort string
	// ServiceTier overrides service_tier. Allowed values are default, priority
	// (Fast), and flex; empty keeps the CLI default. Model support may vary.
	ServiceTier string

	// MCPServers explicitly enables trusted stdio MCP servers. User config remains
	// ignored. Treat commands and environment values as sensitive configuration;
	// overrides are passed in the CLI argument vector. Empty enables no servers.
	MCPServers map[string]MCPServer
}

// MCPServer configures a trusted stdio server, enabled for new and resumed sessions.
// StartupTimeoutSeconds zero keeps the CLI default. All MCP overrides together
// are limited to 1 MiB. Server names use only ASCII letters, digits, '_' and '-'.
// MCP servers run outside the CLI sandbox; configure only trusted executables.
type MCPServer struct {
	Command               string            `json:"command"`
	Args                  []string          `json:"args,omitempty"`
	Env                   map[string]string `json:"env,omitempty"`
	Cwd                   string            `json:"cwd,omitempty"`
	StartupTimeoutSeconds int               `json:"startup_timeout_sec,omitempty"`
}

// Result contains the session ID and the last completed agent message.
// On failure, SessionID is retained when known and Text is empty.
type Result struct {
	SessionID string
	Text      string
}

// Run starts a session when sessionID is empty, otherwise resumes that UUID.
// Prompts are sent verbatim through stdin, never through a shell. Success
// requires a thread.started event, turn.completed, and a successful CLI exit.
// User config and execpolicy rules are ignored; this does not isolate credentials,
// disable project instructions, or replace the CLI's sandbox enforcement.
// exec is noninteractive: approval_policy remains never, which fails requests
// needing escalation, not an interactive approval mechanism. Setting on-request
// would not supply the missing bidirectional request/response channel. MCP tool
// authorization must be enforced separately; this policy is not an MCP allowlist.
// An error or cancellation does not roll back commands or MCP side effects;
// retain the session ID and reconcile uncertain effects before retrying.
func (c *Client) Run(ctx context.Context, sessionID, prompt string) (Result, error) {
	result := Result{SessionID: sessionID}
	sandbox, err := c.ExecutionPolicy.SandboxMode()
	if err != nil {
		return result, err
	}
	if c.ExecutionPolicy == ExecutionAccountAccess {
		return result, ErrInteractiveApprovalRequired
	}
	if sessionID != "" && !validSessionID(sessionID) {
		return result, errors.New("codex: session ID must be a UUID")
	}
	if len(prompt) > maxPrompt {
		return result, fmt.Errorf("codex: prompt exceeds %d bytes", maxPrompt)
	}
	if len(c.Instructions) > maxPrompt {
		return result, fmt.Errorf("codex: instructions exceeds %d bytes", maxPrompt)
	}
	if len(c.OutputSchema) > maxSchema {
		return result, fmt.Errorf("codex: output schema exceeds %d bytes", maxSchema)
	}
	if len(c.OutputSchema) != 0 {
		schema := bytes.TrimSpace(c.OutputSchema)
		if !json.Valid(c.OutputSchema) || schema[0] != '{' {
			return result, errors.New("codex: output schema must be a JSON object")
		}
	}
	if c.Timeout < 0 {
		return result, errors.New("codex: timeout must not be negative")
	}
	switch c.ReasoningEffort {
	case "", "none", "minimal", "low", "medium", "high", "xhigh":
	default:
		return result, fmt.Errorf("codex: invalid reasoning effort %q: want none, minimal, low, medium, high, xhigh, or empty", c.ReasoningEffort)
	}
	switch c.ServiceTier {
	case "", "default", "priority", "flex":
	default:
		return result, fmt.Errorf("codex: invalid service tier %q: want default, priority, flex, or empty", c.ServiceTier)
	}
	mcpArgs, err := mcpOverrides(c.MCPServers)
	if err != nil {
		return result, err
	}
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// Keep internal output-limit cancellation separate from the caller's error.
	runCtx, stop := context.WithCancel(ctx)
	defer stop()

	binary := c.Binary
	if binary == "" {
		binary = "codex"
	}
	dir := c.WorkDir
	if dir == "" {
		dir = "."
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return result, fmt.Errorf("codex: working directory: %w", err)
	}
	args := []string{
		"exec", "--json", "--ignore-user-config", "--ignore-rules",
		"--skip-git-repo-check", "--cd", dir,
		"-c", "sandbox_mode=" + tomlString(sandbox), "-c", `approval_policy="never"`,
	}
	if c.Model != "" {
		args = append(args, "--model="+c.Model)
	}
	if c.ReasoningEffort != "" {
		args = append(args, "-c", `model_reasoning_effort="`+c.ReasoningEffort+`"`)
	}
	if c.ServiceTier != "" {
		args = append(args, "-c", `service_tier="`+c.ServiceTier+`"`)
	}
	if c.Instructions != "" {
		args = append(args, "-c", "developer_instructions="+tomlString(c.Instructions))
	}
	args = append(args, mcpArgs...)
	if len(c.OutputSchema) != 0 {
		// Use the system temporary directory, not the agent's working directory.
		schema, err := os.CreateTemp("", "codex-output-schema-*.json")
		if err != nil {
			return result, fmt.Errorf("codex: create output schema: %w", err)
		}
		defer os.Remove(schema.Name())
		_, writeErr := schema.Write(c.OutputSchema)
		closeErr := schema.Close()
		if err := errors.Join(writeErr, closeErr); err != nil {
			return result, fmt.Errorf("codex: write output schema: %w", err)
		}
		// Keep this exec option before resume (supported by Codex 0.153.1).
		args = append(args, "--output-schema", schema.Name())
	}
	if sessionID != "" {
		args = append(args, "resume", "--", sessionID, "-")
	} else {
		args = append(args, "--", "-")
	}

	cmd := exec.CommandContext(runCtx, binary, args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(prompt)
	cmd.WaitDelay = waitDelay
	cleanup := configureProcessCleanup(cmd)
	defer cleanup()
	stdout := boundedOutput{limit: maxStdout, stop: stop}
	stderr := boundedOutput{limit: maxStderr, stop: stop}
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	parsed, parseErr := parseEvents(stdout.buf.Bytes(), sessionID)
	result.SessionID = parsed.SessionID
	if ctx.Err() != nil {
		return result, fmt.Errorf("codex: %w", ctx.Err())
	}
	if stdout.exceeded || stderr.exceeded {
		return result, fmt.Errorf("codex: output limit exceeded (stdout %d bytes, stderr %d bytes)", maxStdout, maxStderr)
	}
	if parseErr != nil {
		parseErr = errors.New(redactMCPEnv(parseErr.Error(), c.MCPServers))
	}
	if runErr != nil {
		return result, fmt.Errorf("codex: process failed: %w; stderr: %s", errors.Join(runErr, parseErr), redactMCPEnv(strings.TrimSpace(stderr.buf.String()), c.MCPServers))
	}
	if parseErr != nil {
		return result, fmt.Errorf("codex: %w", parseErr)
	}
	return parsed, nil
}

func redactMCPEnv(message string, servers map[string]MCPServer) string {
	for _, server := range servers {
		for _, value := range server.Env {
			if value == "" {
				continue
			}
			quoted := tomlString(value)
			message = strings.ReplaceAll(message, quoted[1:len(quoted)-1], "[REDACTED]")
			message = strings.ReplaceAll(message, value, "[REDACTED]")
		}
	}
	return message
}

func tomlString(value string) string {
	quoted, _ := json.Marshal(value)
	// JSON string escapes are TOML-compatible, but TOML also requires DEL escaped.
	return strings.ReplaceAll(string(quoted), "\x7f", `\u007f`)
}

func mcpOverrides(servers map[string]MCPServer) ([]string, error) {
	const maxMCP = 1 << 20
	// Bound raw input (including entry overhead) before allocating quoted values.
	total := 0
	check := func(value string) bool {
		if len(value) > maxMCP-total || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
			return false
		}
		total += len(value) + 32
		return total <= maxMCP
	}
	names := make([]string, 0)
	for name, server := range servers {
		if name == "" || strings.Trim(name, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-") != "" {
			return nil, errors.New("codex: invalid MCP server name")
		}
		if strings.TrimSpace(server.Command) == "" || server.StartupTimeoutSeconds < 0 {
			return nil, errors.New("codex: MCP command is required and startup timeout must not be negative")
		}
		if !check(name) || !check(server.Command) || !check(server.Cwd) {
			return nil, errors.New("codex: invalid or oversized MCP configuration (limit 1 MiB)")
		}
		for _, arg := range server.Args {
			if !check(arg) {
				return nil, errors.New("codex: invalid or oversized MCP arguments (limit 1 MiB)")
			}
		}
		for key, value := range server.Env {
			if key == "" || strings.ContainsRune(key, '=') || strings.ContainsFunc(key, func(r rune) bool { return r < 32 || r == 127 }) {
				return nil, errors.New("codex: invalid MCP environment key")
			}
			if !check(key) || !check(value) {
				return nil, errors.New("codex: invalid or oversized MCP environment (limit 1 MiB)")
			}
		}
		names = append(names, name)
	}
	slices.Sort(names)
	var args []string
	for _, name := range names {
		server := servers[name]
		prefix := "mcp_servers." + name + "."
		args = append(args, "-c", prefix+"command="+tomlString(server.Command), "-c", prefix+"enabled=true")
		quotedArgs := make([]string, 0, len(server.Args))
		for _, arg := range server.Args {
			quotedArgs = append(quotedArgs, tomlString(arg))
		}
		args = append(args, "-c", prefix+"args=["+strings.Join(quotedArgs, ",")+"]")
		if len(server.Env) != 0 {
			keys := make([]string, 0, len(server.Env))
			for key := range server.Env {
				keys = append(keys, key)
			}
			slices.Sort(keys)
			entries := make([]string, 0, len(keys))
			for _, key := range keys {
				entries = append(entries, tomlString(key)+"="+tomlString(server.Env[key]))
			}
			// The CLI splits override paths on dots without parsing quoted keys.
			// An inline table keeps environment keys literal, including dots.
			args = append(args, "-c", prefix+"env={"+strings.Join(entries, ",")+"}")
		}
		if server.Cwd != "" {
			args = append(args, "-c", prefix+"cwd="+tomlString(server.Cwd))
		}
		if server.StartupTimeoutSeconds != 0 {
			args = append(args, "-c", prefix+"startup_timeout_sec="+strconv.Itoa(server.StartupTimeoutSeconds))
		}
	}
	total = 0
	for _, arg := range args {
		total += len(arg) + 1
		if total > maxMCP {
			return nil, errors.New("codex: MCP overrides exceed 1 MiB")
		}
	}
	return args, nil
}

func validSessionID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if id[i] != '-' {
				return false
			}
		} else if !strings.ContainsRune("0123456789abcdefABCDEF", rune(id[i])) {
			return false
		}
	}
	return true
}

// Each output has one os/exec copying goroutine; read it only after Run returns.
// Cancel on overflow rather than merely dropping an unbounded output stream.
type boundedOutput struct {
	buf      bytes.Buffer
	limit    int
	stop     context.CancelFunc
	exceeded bool
}

func (w *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := w.limit - w.buf.Len()
	if n > remaining {
		w.buf.Write(p[:remaining])
		w.exceeded = true
		w.stop()
		return remaining, errors.New("codex: output limit exceeded")
	}
	return w.buf.Write(p)
}

func parseEvents(data []byte, sessionID string) (Result, error) {
	result := Result{SessionID: sessionID}
	var started, completed bool
	var text string
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), maxStdout+1)
	for scanner.Scan() {
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		var event struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
			Message  string `json:"message"`
			Item     struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return result, fmt.Errorf("invalid JSON event: %w", err)
		}
		if event.Type == "" {
			return result, errors.New("event missing type")
		}
		switch event.Type {
		case "thread.started":
			if started || !validSessionID(event.ThreadID) {
				return result, errors.New("invalid thread.started event")
			}
			if sessionID != "" && !strings.EqualFold(sessionID, event.ThreadID) {
				return result, errors.New("resumed thread does not match requested session")
			}
			result.SessionID = event.ThreadID
			started = true
		case "turn.started":
			if !started || completed {
				return result, errors.New("unexpected turn.started event")
			}
		case "item.completed":
			if event.Item.Type == "agent_message" {
				if !started || completed {
					return result, errors.New("agent message outside active turn")
				}
				text = event.Item.Text
			}
		case "turn.completed":
			if !started || completed {
				return result, errors.New("unexpected turn.completed event")
			}
			completed = true
		case "turn.failed":
			return result, fmt.Errorf("turn failed: %s", event.Error.Message)
		case "error":
			// The CLI also emits this event for recoverable transport errors.
			// turn.failed, a missing completion, or process exit decide failure.
			continue
		}
	}
	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("read events: %w", err)
	}
	if !started || !completed {
		return result, errors.New("missing thread.started or turn.completed event")
	}
	result.Text = text
	return result, nil
}
