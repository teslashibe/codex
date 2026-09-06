package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// InteractiveConfigurationError means the deployment configuration did not
// match its explicit review. Never retry with a different home or weaker guards.
type InteractiveConfigurationError struct{ Reason string }

func (e *InteractiveConfigurationError) Error() string {
	return "codex: interactive configuration blocked: " + e.Reason
}

// ApprovalDecision is per-request only. Its zero value declines. No session
// acceptance, policy amendment, permission grant, or persistent approval exists.
type ApprovalDecision uint8

const (
	ApprovalDeny ApprovalDecision = iota
	ApprovalOnce
	ApprovalCancel
)

type ApprovalKind string

const (
	ApprovalCommand    ApprovalKind = "commandExecution"
	ApprovalFileChange ApprovalKind = "fileChange"
	ApprovalMCP        ApprovalKind = "mcpElicitation"
)

// ApprovalRequest contains untrusted server/model data, never authorization.
// RequestID preserves the JSON string/integer ID, including its wire type.
// Exactly one of Command, FileChange, or MCP is populated. ThreadID and TurnID
// have been correlated to this run. Authenticate the human separately.
type ApprovalRequest struct {
	RequestID                        json.RawMessage
	ThreadID, TurnID, ItemID, Reason string
	Kind                             ApprovalKind
	Command                          *CommandApproval
	FileChange                       *FileChangeApproval
	MCP                              *MCPApproval
}
type CommandApproval struct{ Command, Cwd, EnvironmentID, ApprovalID string }
type FileChangeApproval struct{ Changes []FileChange }
type FileChange struct {
	Path string          `json:"path"`
	Kind json.RawMessage `json:"kind"`
	Diff string          `json:"diff"`
}

// MCPApproval currently supports only plain empty confirmation forms. URL,
// extended semantic forms, input schemas and persistence metadata are declined;
// they need a separately reviewed renderer/validator, not a generic approve button.
type MCPApproval struct {
	ServerName, Message string
	// ToolName and Arguments are populated only when native approval metadata
	// matches exactly one active tool call in this thread and turn.
	ToolName  string
	Arguments json.RawMessage
}

// ApprovalHandler must honor ctx cancellation and may retain only copied data.
// Return only an explicit authenticated per-request decision. A nil handler,
// error, invalid decision, expired turn, or unsupported request never accepts.
// A callback does NOT guarantee all commands or MCP calls require approval:
// app-server can execute sandbox-allowed actions without emitting requests.
type ApprovalHandler func(context.Context, ApprovalRequest) (ApprovalDecision, error)

// InteractiveError retains whether a turn may have produced external effects.
// Even denial, cancellation and transport failure cannot undo previous tools.
// When MayHaveSideEffects is true, reconcile the session before retrying.
type InteractiveError struct {
	Cause              error
	MayHaveSideEffects bool
}

func (e *InteractiveError) Error() string { return "codex: interactive run: " + e.Cause.Error() }
func (e *InteractiveError) Unwrap() error { return e.Cause }

// RunInteractive validates a reviewed deployment before starting app-server.
// On-request handles only approvals emitted by Codex, not every command. Caller
// authentication/authorization and the high-impact approval bridge are separate.
// Run's existing exec/browser path is unchanged. A nil handler denies requests.
func (c *Client) RunInteractive(ctx context.Context, sessionID, prompt string, handler ApprovalHandler, bindings ...MCPBinding) (Result, error) {
	args, err := c.validateInteractiveConfig(ctx)
	if err != nil {
		return Result{SessionID: sessionID}, err
	}
	servers, err := c.bindInteractiveMCP(bindings)
	if err != nil {
		return Result{SessionID: sessionID}, err
	}
	run := *c
	run.MCPServers = servers
	return run.runInteractive(ctx, sessionID, prompt, handler, args...)
}

const maxRPCMessage = 2 << 20

type rpcMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}
type rpcInput struct {
	message rpcMessage
	err     error
}
type approvalAnswer struct {
	decision ApprovalDecision
	err      error
}
type pendingApproval struct {
	id     json.RawMessage
	mcp    bool
	cancel context.CancelFunc
	answer <-chan approvalAnswer
}

// runInteractive consumes only generated, validated parity overrides.
func (c *Client) runInteractive(ctx context.Context, sessionID, prompt string, handler ApprovalHandler, parityArgs ...string) (result Result, runErr error) {
	result.SessionID = sessionID
	sandbox, err := c.ExecutionPolicy.SandboxMode()
	if err != nil {
		return result, err
	}
	if c.ExecutionPolicy == ExecutionAccountAccess && (c.InteractiveConfig == nil || handler == nil) {
		return result, ErrInteractiveApprovalRequired
	}
	if sessionID != "" && !validSessionID(sessionID) {
		return result, errors.New("codex: session ID must be a UUID")
	}
	if len(prompt) > maxPrompt || len(c.Instructions) > maxPrompt || len(c.OutputSchema) > maxSchema || c.Timeout < 0 {
		return result, errors.New("codex: invalid interactive input bounds")
	}
	if len(c.OutputSchema) > 0 && (!json.Valid(c.OutputSchema) || bytes.TrimSpace(c.OutputSchema)[0] != '{') {
		return result, errors.New("codex: output schema must be an object")
	}
	switch c.ReasoningEffort {
	case "", "none", "minimal", "low", "medium", "high", "xhigh":
	default:
		return result, errors.New("codex: invalid reasoning effort")
	}
	switch c.ServiceTier {
	case "", "default", "priority", "flex":
	default:
		return result, errors.New("codex: invalid service tier")
	}
	mcpArgs, err := mcpOverrides(c.MCPServers)
	if err != nil {
		return result, err
	}
	dir := c.WorkDir
	if dir == "" {
		dir = "."
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return result, err
	}
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	processCtx, stop := context.WithCancel(ctx)
	defer stop()
	binary := c.Binary
	if binary == "" {
		binary = "codex"
	}
	// Parity overrides disable only reviewed ambient additions, then explicit
	// server definitions are reapplied unchanged. Approval never is not used.
	args := []string{"app-server", "--stdio", "-c", "sandbox_mode=" + tomlString(sandbox), "-c", `approval_policy="on-request"`, "-c", `approvals_reviewer="user"`}
	args = append(args, parityArgs...)
	args = append(args, mcpArgs...)
	cmd := exec.CommandContext(processCtx, binary, args...)
	cmd.Dir, cmd.WaitDelay = dir, waitDelay
	cleanup := configureProcessCleanup(cmd)
	defer cleanup()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return result, err
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return result, err
	}
	defer stdout.Close()
	stderr := boundedOutput{limit: maxStderr, stop: stop}
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return result, fmt.Errorf("codex: app-server start: %w", err)
	}
	incoming := make(chan rpcInput, 1)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		defer close(incoming)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 4096), maxRPCMessage+1)
		total := 0
		send := func(input rpcInput) bool {
			select {
			case incoming <- input:
				return true
			case <-processCtx.Done():
				return false
			}
		}
		for scanner.Scan() {
			line := scanner.Bytes()
			total += len(line) + 1
			if len(line) > maxRPCMessage || total > maxStdout {
				send(rpcInput{err: errors.New("RPC output limit exceeded")})
				return
			}
			var msg rpcMessage
			if err := json.Unmarshal(line, &msg); err != nil || len(bytes.TrimSpace(line)) == 0 || bytes.TrimSpace(line)[0] != '{' {
				send(rpcInput{err: errors.New("malformed RPC message")})
				return
			}
			if !send(rpcInput{message: msg}) {
				return
			}
		}
		if scanner.Err() != nil {
			send(rpcInput{err: errors.New("RPC read failure or message limit exceeded")})
		}
	}()
	var pending *pendingApproval
	var mayHaveEffects bool
	defer func() {
		if pending != nil {
			pending.cancel()
		}
		stop()
		_ = stdin.Close()
		cleanup()
		_ = stdout.Close()
		<-readerDone
		_ = cmd.Wait()
		if runErr == nil && (ctx.Err() != nil || stderr.exceeded) {
			if ctx.Err() != nil {
				runErr = ctx.Err()
			} else {
				runErr = errors.New("app-server stderr limit exceeded")
			}
		}
		if runErr != nil {
			result.Text = ""
			// Do not echo arbitrary stderr, remote errors, or callback errors: they can
			// contain credentials not present in MCP Env. Retain only bounded metadata.
			if stderr.buf.Len() > 0 {
				runErr = fmt.Errorf("%w (app-server stderr: %d bytes withheld)", runErr, stderr.buf.Len())
			}
			runErr = &InteractiveError{Cause: runErr, MayHaveSideEffects: mayHaveEffects}
		}
	}()
	send := func(value any) error {
		data, err := json.Marshal(value)
		if err != nil || len(data) > maxRPCMessage {
			return errors.New("RPC request exceeds message limit")
		}
		_, err = stdin.Write(append(data, '\n'))
		if err != nil {
			return errors.New("RPC write failed")
		}
		return nil
	}
	request := func(id int, method string, params any) error {
		return send(struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
			Params any    `json:"params"`
		}{id, method, params})
	}
	reply := func(id json.RawMessage, mcp bool, decision ApprovalDecision) error {
		value := "decline"
		if decision == ApprovalOnce {
			value = "accept"
		}
		if decision == ApprovalCancel {
			value = "cancel"
		}
		var body any = map[string]any{"decision": value}
		if mcp {
			var content any
			if decision == ApprovalOnce {
				content = map[string]any{}
			}
			body = map[string]any{"action": value, "content": content, "_meta": nil}
		}
		return send(struct {
			ID     json.RawMessage `json:"id"`
			Result any             `json:"result"`
		}{id, body})
	}
	if err := request(1, "initialize", map[string]any{"clientInfo": map[string]string{"name": "codex_go", "version": "0.1"}, "capabilities": map[string]bool{"experimentalApi": false}}); err != nil {
		return result, err
	}
	stage := 1
	threadResponse, threadNotification := false, false
	thread, turn, text := "", "", ""
	completed := false
	seenRequests := map[string]bool{}
	items := map[string]json.RawMessage{}
	for {
		var answer <-chan approvalAnswer
		if pending != nil {
			answer = pending.answer
		}
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-processCtx.Done():
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			return result, errors.New("app-server output limit or process cancellation")
		case a := <-answer:
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			decision := a.decision
			if a.err != nil || decision > ApprovalCancel {
				decision = ApprovalDeny
			}
			p := pending
			pending = nil
			p.cancel()
			if err := reply(p.id, p.mcp, decision); err != nil {
				return result, err
			}
			if decision == ApprovalCancel {
				return result, context.Canceled
			}
		case input, ok := <-incoming:
			if !ok {
				return result, errors.New("app-server ended before turn completion")
			}
			if input.err != nil {
				return result, input.err
			}
			msg := input.message
			if len(msg.ID) > 0 && !validRPCID(msg.ID) {
				return result, errors.New("invalid RPC ID")
			}
			if msg.Method != "" {
				if len(msg.Result) > 0 || len(msg.Error) > 0 {
					return result, errors.New("ambiguous RPC envelope")
				}
				if len(msg.ID) > 0 {
					key := string(msg.ID)
					if seenRequests[key] {
						return result, errors.New("duplicate server request ID")
					}
					seenRequests[key] = true
					req, supported := decodeApproval(msg, thread, turn, items)
					isMCP := msg.Method == "mcpServer/elicitation/request"
					if !supported || completed || pending != nil || handler == nil {
						if isMCP || msg.Method == "item/commandExecution/requestApproval" || msg.Method == "item/fileChange/requestApproval" {
							if err := reply(msg.ID, isMCP, ApprovalDeny); err != nil {
								return result, err
							}
						} else {
							// Unknown methods can request sticky permissions or host-side tools.
							// Never guess a successful response shape; terminate after an error.
							_ = send(map[string]any{"id": msg.ID, "error": map[string]any{"code": -32601, "message": "unsupported server request"}})
							return result, errors.New("unsupported server request")
						}
						continue
					}
					approvalCtx, approvalCancel := context.WithCancel(ctx)
					answers := make(chan approvalAnswer, 1)
					pending = &pendingApproval{id: append(json.RawMessage(nil), msg.ID...), mcp: isMCP, cancel: approvalCancel, answer: answers}
					go func() { d, e := handler(approvalCtx, req); answers <- approvalAnswer{d, e} }()
					continue
				}
				var p struct {
					ThreadID  string          `json:"threadId"`
					TurnID    string          `json:"turnId"`
					RequestID json.RawMessage `json:"requestId"`
					Thread    struct {
						ID string `json:"id"`
					} `json:"thread"`
					Turn struct {
						ID     string `json:"id"`
						Status string `json:"status"`
					} `json:"turn"`
					Item json.RawMessage `json:"item"`
				}
				if err := json.Unmarshal(msg.Params, &p); err != nil {
					return result, errors.New("malformed notification")
				}
				switch msg.Method {
				case "thread/started":
					if stage < 2 || threadNotification || !validSessionID(p.Thread.ID) || (sessionID != "" && !strings.EqualFold(sessionID, p.Thread.ID)) {
						return result, errors.New("unexpected thread notification")
					}
					if thread != "" && thread != p.Thread.ID {
						return result, errors.New("thread mismatch")
					}
					threadNotification = true
					thread = p.Thread.ID
					result.SessionID = thread
				case "turn/started":
					if stage < 3 || stage > 4 || completed || p.ThreadID != thread || p.Turn.ID == "" || (turn != "" && turn != p.Turn.ID) {
						return result, errors.New("unexpected turn notification")
					}
					turn = p.Turn.ID
				case "item/started", "item/completed":
					if stage < 3 || p.ThreadID != thread || turn == "" || p.TurnID != turn || completed {
						return result, errors.New("item outside active turn")
					}
					var item struct{ ID, Type, Text string }
					if err := json.Unmarshal(p.Item, &item); err != nil || item.ID == "" {
						return result, errors.New("invalid item")
					}
					if msg.Method == "item/started" {
						items[item.ID] = append(json.RawMessage(nil), p.Item...)
					} else {
						delete(items, item.ID)
						if item.Type == "agentMessage" {
							text = item.Text
						}
					}
				case "serverRequest/resolved":
					if p.ThreadID != thread {
						return result, errors.New("resolved request thread mismatch")
					}
					if pending != nil && bytes.Equal(p.RequestID, pending.id) {
						pending.cancel()
						pending = nil
					}
				case "turn/completed":
					if stage < 3 || p.ThreadID != thread || turn == "" || p.Turn.ID != turn || completed {
						return result, errors.New("unexpected turn completion")
					}
					if pending != nil {
						pending.cancel()
						pending = nil
						return result, errors.New("turn completed with pending approval")
					}
					if p.Turn.Status != "completed" {
						return result, errors.New("turn failed or interrupted")
					}
					completed = true
				}
			} else {
				if len(msg.ID) == 0 || (len(msg.Result) == 0) == (len(msg.Error) == 0) || string(msg.ID) != fmt.Sprint(stage) {
					return result, errors.New("unexpected RPC response")
				}
				if len(msg.Error) > 0 {
					return result, errors.New("app-server RPC error (details withheld)")
				}
				switch stage {
				case 1:
					var initialized struct {
						UserAgent string `json:"userAgent"`
					}
					if json.Unmarshal(msg.Result, &initialized) != nil || initialized.UserAgent == "" {
						return result, errors.New("invalid initialize response")
					}
					if err := send(map[string]any{"method": "initialized"}); err != nil {
						return result, err
					}
					params := map[string]any{"cwd": dir, "sandbox": sandbox, "approvalPolicy": "on-request", "approvalsReviewer": "user"}
					if c.Model != "" {
						params["model"] = c.Model
					}
					if c.Instructions != "" {
						params["developerInstructions"] = c.Instructions
					}
					method := "thread/start"
					if sessionID != "" {
						method = "thread/resume"
						params["threadId"] = sessionID
						params["excludeTurns"] = true
					}
					stage = 2
					if err := request(2, method, params); err != nil {
						return result, err
					}
				case 2:
					if threadResponse {
						return result, errors.New("duplicate thread response")
					}
					var response struct {
						Thread struct {
							ID string `json:"id"`
						} `json:"thread"`
						ApprovalPolicy    string `json:"approvalPolicy"`
						ApprovalsReviewer string `json:"approvalsReviewer"`
						Sandbox           struct {
							Type          string   `json:"type"`
							NetworkAccess bool     `json:"networkAccess"`
							WritableRoots []string `json:"writableRoots"`
						} `json:"sandbox"`
					}
					if json.Unmarshal(msg.Result, &response) != nil || !validSessionID(response.Thread.ID) || response.ApprovalPolicy != "on-request" || response.ApprovalsReviewer != "user" {
						return result, errors.New("invalid thread or effective approval policy")
					}
					expectedSandbox := "readOnly"
					if sandbox == "workspace-write" {
						expectedSandbox = "workspaceWrite"
					}
					if sandbox == "danger-full-access" {
						expectedSandbox = "dangerFullAccess"
					}
					if response.Sandbox.Type != expectedSandbox || response.Sandbox.NetworkAccess || len(response.Sandbox.WritableRoots) != 0 {
						return result, errors.New("unexpected effective sandbox permissions")
					}
					if (sessionID != "" && !strings.EqualFold(sessionID, response.Thread.ID)) || (thread != "" && thread != response.Thread.ID) {
						return result, errors.New("thread mismatch")
					}
					thread = response.Thread.ID
					result.SessionID = thread
					threadResponse = true
				case 3:
					var response struct {
						Turn struct {
							ID string `json:"id"`
						} `json:"turn"`
					}
					if json.Unmarshal(msg.Result, &response) != nil || response.Turn.ID == "" || (turn != "" && response.Turn.ID != turn) {
						return result, errors.New("turn mismatch")
					}
					turn = response.Turn.ID
					stage = 4
				default:
					return result, errors.New("unexpected RPC response")
				}
			}
			// Fresh thread/start response and thread/started can arrive in either
			// order. Submit no turn until both match. Resume need not emit started.
			if stage == 2 && threadResponse && (sessionID != "" || threadNotification) {
				params := map[string]any{"threadId": thread, "input": []any{map[string]string{"type": "text", "text": prompt}}, "approvalPolicy": "on-request", "approvalsReviewer": "user", "cwd": dir}
				if c.ReasoningEffort != "" {
					params["effort"] = c.ReasoningEffort
				}
				if c.ServiceTier != "" {
					params["serviceTier"] = c.ServiceTier
				}
				if len(c.OutputSchema) > 0 {
					params["outputSchema"] = c.OutputSchema
				}
				stage = 3
				mayHaveEffects = true
				if err := request(3, "turn/start", params); err != nil {
					return result, err
				}
			}
			if completed && stage == 4 {
				result.Text = text
				return result, nil
			}
		}
	}
}

func validRPCID(id json.RawMessage) bool {
	var s string
	if json.Unmarshal(id, &s) == nil {
		return s != ""
	}
	var n int64
	return json.Unmarshal(id, &n) == nil && string(id) != "null"
}

// Unknown fields fail closed on approval requests. Version changes must be
// reviewed rather than losing potentially security-relevant request context.
func decodeApproval(msg rpcMessage, thread, turn string, items map[string]json.RawMessage) (ApprovalRequest, bool) {
	req := ApprovalRequest{RequestID: append(json.RawMessage(nil), msg.ID...)}
	var p struct {
		ThreadID          string          `json:"threadId"`
		TurnID            string          `json:"turnId"`
		ItemID            string          `json:"itemId"`
		Reason            string          `json:"reason"`
		StartedAtMs       int64           `json:"startedAtMs"`
		Kind              string          `json:"kind"`
		Command           string          `json:"command"`
		Cwd               string          `json:"cwd"`
		EnvironmentID     string          `json:"environmentId"`
		ApprovalID        string          `json:"approvalId"`
		CommandActions    json.RawMessage `json:"commandActions"`
		Network           json.RawMessage `json:"networkApprovalContext"`
		ExecAmendment     json.RawMessage `json:"proposedExecpolicyAmendment"`
		NetworkAmendments json.RawMessage `json:"proposedNetworkPolicyAmendments"`
		GrantRoot         *string         `json:"grantRoot"`
		ServerName        string          `json:"serverName"`
		Mode              string          `json:"mode"`
		Message           string          `json:"message"`
		Schema            json.RawMessage `json:"requestedSchema"`
		Meta              json.RawMessage `json:"_meta"`
		URL               string          `json:"url"`
		ElicitationID     string          `json:"elicitationId"`
	}
	decoder := json.NewDecoder(bytes.NewReader(msg.Params))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&p) != nil || thread == "" || turn == "" || p.ThreadID != thread || p.TurnID != turn {
		return req, false
	}
	present := func(v json.RawMessage) bool { return len(v) > 0 && string(v) != "null" }
	if present(p.Network) || present(p.ExecAmendment) || present(p.NetworkAmendments) || p.GrantRoot != nil || (present(p.Meta) && msg.Method != "mcpServer/elicitation/request") {
		return req, false
	}
	req.ThreadID, req.TurnID, req.ItemID, req.Reason = p.ThreadID, p.TurnID, p.ItemID, p.Reason
	switch msg.Method {
	case "item/commandExecution/requestApproval":
		if (p.Kind != "" && p.Kind != "command") || p.Command == "" || p.Cwd == "" || p.ItemID == "" {
			return req, false
		}
		req.Kind = ApprovalCommand
		req.Command = &CommandApproval{p.Command, p.Cwd, p.EnvironmentID, p.ApprovalID}
	case "item/fileChange/requestApproval":
		var item struct {
			Type    string       `json:"type"`
			Changes []FileChange `json:"changes"`
		}
		if json.Unmarshal(items[p.ItemID], &item) != nil || item.Type != "fileChange" || len(item.Changes) == 0 {
			return req, false
		}
		for _, change := range item.Changes {
			if change.Path == "" || len(change.Kind) == 0 {
				return req, false
			}
		}
		req.Kind = ApprovalFileChange
		req.FileChange = &FileChangeApproval{item.Changes}
	case "mcpServer/elicitation/request":
		// No user-input renderer or JSON-schema validator: accept only this exact
		// empty object schema, never an extended form or URL login flow.
		var schema map[string]json.RawMessage
		if p.Mode != "form" || p.URL != "" || p.ElicitationID != "" || p.ServerName == "" || json.Unmarshal(p.Schema, &schema) != nil || len(schema) != 2 || string(schema["type"]) != `"object"` || string(bytes.TrimSpace(schema["properties"])) != "{}" {
			return req, false
		}
		req.Kind = ApprovalMCP
		req.MCP = &MCPApproval{ServerName: p.ServerName, Message: p.Message}
		if present(p.Meta) {
			var meta struct {
				Kind        string          `json:"codex_approval_kind"`
				Description string          `json:"tool_description"`
				Params      json.RawMessage `json:"tool_params"`
				Display     json.RawMessage `json:"tool_params_display"`
				Persist     []string        `json:"persist"`
			}
			decoder := json.NewDecoder(bytes.NewReader(p.Meta))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&meta) != nil || meta.Kind != "mcp_tool_call" {
				return req, false
			}
			// These are offered UI choices, not required grants. This client
			// never returns persistence metadata: ApprovalOnce stays one call.
			for _, scope := range meta.Persist {
				if scope != "session" && scope != "always" {
					return req, false
				}
			}
			var params map[string]json.RawMessage
			if json.Unmarshal(meta.Params, &params) != nil || params == nil {
				return req, false
			}
			canonical, err := json.Marshal(params)
			if err != nil {
				return req, false
			}
			for id, raw := range items {
				var item struct {
					ID, Type, Server, Tool, Status string
					Arguments                      map[string]json.RawMessage
				}
				if json.Unmarshal(raw, &item) != nil || item.Type != "mcpToolCall" || item.Server != p.ServerName || item.Status != "inProgress" {
					continue
				}
				// Multiple active calls on this server are ambiguous even if only
				// one argument object matches: the request has no native item ID.
				if req.ItemID != "" || item.ID != id || item.Tool == "" {
					return req, false
				}
				arguments, err := json.Marshal(item.Arguments)
				if err != nil || !bytes.Equal(arguments, canonical) {
					return req, false
				}
				req.ItemID = id
				req.MCP.ToolName = item.Tool
				req.MCP.Arguments = append(json.RawMessage(nil), meta.Params...)
			}
			if req.ItemID == "" || req.MCP.ToolName == "" {
				return req, false
			}
		}
	default:
		return req, false
	}
	return req, true
}
