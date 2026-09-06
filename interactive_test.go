package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func runRPCFixture(scenario string) {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		version := "0.153.1"
		if scenario == "version-drift" {
			version = "0.154.0"
		}
		fmt.Println("codex-cli " + version)
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), maxRPCMessage+1)
	enc := json.NewEncoder(os.Stdout)
	read := func() map[string]json.RawMessage {
		if !scanner.Scan() {
			os.Exit(8)
		}
		var msg map[string]json.RawMessage
		if json.Unmarshal(scanner.Bytes(), &msg) != nil {
			os.Exit(9)
		}
		return msg
	}
	send := func(value any) {
		if enc.Encode(value) != nil {
			os.Exit(10)
		}
	}
	response := func(id any, result any) { send(map[string]any{"id": id, "result": result}) }
	notify := func(method string, params any) { send(map[string]any{"method": method, "params": params}) }
	capture := func(msg any) {
		path := os.Getenv("CODEX_RPC_CAPTURE")
		if path != "" {
			f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
			if e != nil {
				os.Exit(11)
			}
			_ = json.NewEncoder(f).Encode(msg)
			_ = f.Close()
		}
	}
	capture(map[string]any{"args": os.Args[1:], "home": os.Getenv("CODEX_HOME")})
	init := read()
	capture(init)
	if scenario == "malformed" {
		fmt.Println("not json")
		return
	}
	if scenario == "oversized" {
		fmt.Println(strings.Repeat("x", maxRPCMessage+2))
		return
	}
	if scenario == "stderr" {
		fmt.Fprintln(os.Stderr, "SECRET_NOT_TO_LOG")
		return
	}
	if scenario == "stderr-limit" {
		fmt.Fprintln(os.Stderr, strings.Repeat("x", maxStderr+1))
		time.Sleep(time.Second)
		return
	}
	if scenario == "init-error" {
		send(map[string]any{"id": 1, "error": map[string]any{"code": -1, "message": "SECRET_NOT_TO_LOG"}})
		return
	}
	if scenario == "wrong-id" {
		response("1", map[string]any{})
		return
	}
	response(1, map[string]any{"userAgent": "codex/0.153.1", "codexHome": "/unchanged/home", "platformFamily": "unix", "platformOs": "macos"})
	capture(read()) // initialized notification
	threadRequest := read()
	capture(threadRequest)
	if scenario == "thread-error" {
		send(map[string]any{"id": 2, "error": map[string]any{"code": -1}})
		return
	}
	policy := "on-request"
	if scenario == "never" || scenario == "yolo" || scenario == "yolo-mcp" {
		policy = "never"
	}
	thread := testSession
	if scenario == "wrong-thread" {
		thread = "019cb612-9a00-7000-8000-000000000002"
	}
	fresh := string(threadRequest["method"]) == `"thread/start"`
	started := func() { notify("thread/started", map[string]any{"thread": map[string]string{"id": thread}}) }
	if fresh && scenario == "notification-first" {
		started()
	}
	if !fresh {
		var p map[string]json.RawMessage
		_ = json.Unmarshal(threadRequest["params"], &p)
		if string(p["excludeTurns"]) != "true" {
			os.Exit(12)
		}
	}
	sandbox := "readOnly"
	if scenario == "account" || scenario == "yolo" || scenario == "yolo-mcp" {
		sandbox = "dangerFullAccess"
	}
	if scenario == "workspace" {
		sandbox = "workspaceWrite"
	}
	if scenario == "sandbox-broadened" {
		sandbox = "dangerFullAccess"
	}
	reviewer := "user"
	if scenario == "reviewer-broadened" {
		reviewer = "guardian_subagent"
	}
	response(2, map[string]any{"thread": map[string]string{"id": thread}, "approvalPolicy": policy, "approvalsReviewer": reviewer, "sandbox": map[string]string{"type": sandbox}})
	if fresh && scenario != "notification-first" {
		if scenario == "missing-started" {
			time.Sleep(20 * time.Second)
			return
		}
		if scenario == "mismatch-started" {
			thread = "019cb612-9a00-7000-8000-000000000003"
		}
		started()
		if scenario == "duplicate-started" {
			started()
		}
	}
	if scenario == "never" || scenario == "wrong-thread" {
		time.Sleep(time.Second)
		return
	}
	capture(read()) // turn/start
	if scenario == "turn-error" {
		send(map[string]any{"id": 3, "error": map[string]any{"code": -1}})
		return
	}
	if scenario != "early-complete" {
		response(3, map[string]any{"turn": map[string]string{"id": "turn-1", "status": "inProgress"}})
	}
	notify("turn/started", map[string]any{"threadId": thread, "turn": map[string]string{"id": "turn-1", "status": "inProgress"}})
	if scenario == "hang" {
		time.Sleep(20 * time.Second)
		return
	}
	command := func(id any, turn string, extra map[string]any) {
		p := map[string]any{"threadId": thread, "turnId": turn, "itemId": "cmd-1", "command": "printf hello", "cwd": "/workspace", "kind": "command", "startedAtMs": 1}
		for k, v := range extra {
			p[k] = v
		}
		send(map[string]any{"id": id, "method": "item/commandExecution/requestApproval", "params": p})
	}
	switch scenario {
	case "command", "deny", "cancel", "callback-error", "invalid-decision", "callback-timeout", "multiple", "resolved", "pending-complete", "interleaved", "duplicate":
		command("approval-1", "turn-1", nil)
		if scenario == "resolved" {
			notify("serverRequest/resolved", map[string]any{"threadId": thread, "requestId": "approval-1"})
		} else if scenario == "pending-complete" {
			notify("turn/completed", map[string]any{"threadId": thread, "turn": map[string]string{"id": "turn-1", "status": "completed"}})
			time.Sleep(time.Second)
			return
		} else {
			if scenario == "interleaved" {
				command(52, "turn-1", nil)
			}
			capture(read())
			if scenario == "interleaved" {
				capture(read())
			}
			if scenario == "multiple" {
				command(42, "turn-1", nil)
				capture(read())
			}
			if scenario == "duplicate" {
				command("approval-1", "turn-1", nil)
				time.Sleep(time.Second)
				return
			}
		}
	case "stale":
		command(7, "stale-turn", nil)
		capture(read())
	case "amendment":
		command(7, "turn-1", map[string]any{"proposedExecpolicyAmendment": []string{"printf"}})
		capture(read())
	case "unknown-field":
		command(7, "turn-1", map[string]any{"additionalPermissions": map[string]bool{"all": true}})
		capture(read())
	case "stdin":
		command(7, "turn-1", map[string]any{"kind": "writeStdin"})
		capture(read())
	case "permissions":
		send(map[string]any{"id": 7, "method": "item/permissions/requestApproval", "params": map[string]any{"threadId": thread, "turnId": "turn-1"}})
		capture(read())
		return
	case "file", "grant-root":
		notify("item/started", map[string]any{"threadId": thread, "turnId": "turn-1", "item": map[string]any{"id": "file-1", "type": "fileChange", "changes": []any{map[string]any{"path": "/workspace/a.go", "kind": map[string]string{"type": "update"}, "diff": "-old\n+new"}}}})
		p := map[string]any{"threadId": thread, "turnId": "turn-1", "itemId": "file-1", "startedAtMs": 1}
		if scenario == "grant-root" {
			p["grantRoot"] = "/"
		}
		send(map[string]any{"id": 8, "method": "item/fileChange/requestApproval", "params": p})
		capture(read())
		notify("item/completed", map[string]any{"threadId": thread, "turnId": "turn-1", "item": map[string]any{"id": "file-1", "type": "fileChange"}})
	case "mcp", "mcp-input", "mcp-persist", "mcp-null-turn", "mcp-url", "yolo-mcp":
		p := map[string]any{"threadId": thread, "turnId": "turn-1", "serverName": "existing-browser", "mode": "form", "message": "Allow this action?", "requestedSchema": map[string]any{"type": "object", "properties": map[string]any{}}}
		if scenario == "mcp-input" {
			p["requestedSchema"] = map[string]any{"type": "object", "properties": map[string]any{"token": map[string]string{"type": "string"}}}
		}
		if scenario == "mcp-persist" {
			p["_meta"] = map[string]string{"persist": "session"}
		}
		if scenario == "mcp-null-turn" {
			p["turnId"] = nil
		}
		if scenario == "mcp-url" {
			p["mode"] = "url"
			p["url"] = "https://example.com/login"
			p["elicitationId"] = "login"
			delete(p, "requestedSchema")
		}
		send(map[string]any{"id": "mcp-1", "method": "mcpServer/elicitation/request", "params": p})
		capture(read())
	}
	if scenario == "stale-item" {
		notify("item/completed", map[string]any{"threadId": thread, "turnId": "other", "item": map[string]string{"id": "answer", "type": "agentMessage", "text": "bad"}})
		return
	}
	notify("thread/tokenUsage/updated", map[string]any{"threadId": thread, "turnId": "turn-1"})
	notify("item/completed", map[string]any{"threadId": thread, "turnId": "turn-1", "item": map[string]string{"id": "answer", "type": "agentMessage", "text": "interactive answer"}})
	status := "completed"
	if scenario == "failed" {
		status = "failed"
	}
	notify("turn/completed", map[string]any{"threadId": thread, "turn": map[string]string{"id": "turn-1", "status": status}})
	if scenario == "early-complete" {
		response(3, map[string]any{"turn": map[string]string{"id": "turn-1", "status": "inProgress"}})
	}
	// app-server is long-lived. The runner must close/kill it after this turn.
	for scanner.Scan() {
	}
}

func rpcClient(t *testing.T, scenario string) *Client {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_RPC_TEST_HELPER", scenario)
	return &Client{Binary: binary, WorkDir: t.TempDir(), Timeout: 2 * time.Second}
}
func rpcCapture(t *testing.T, path string) []map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var result []map[string]json.RawMessage
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var msg map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatal(err)
		}
		result = append(result, msg)
	}
	return result
}

func TestRunInteractiveConfigurationBlocked(t *testing.T) {
	for _, policy := range []ExecutionPolicy{"", ExecutionWorkspaceWrite, ExecutionAccountAccess} {
		c := rpcClient(t, "command")
		c.ExecutionPolicy = policy
		capture := filepath.Join(t.TempDir(), "capture")
		t.Setenv("CODEX_RPC_CAPTURE", capture)
		result, err := c.RunInteractive(context.Background(), testSession, "do not execute", func(context.Context, ApprovalRequest) (ApprovalDecision, error) {
			t.Error("unexpected callback")
			return ApprovalOnce, nil
		})
		var blocker *InteractiveConfigurationError
		if !errors.As(err, &blocker) || result != (Result{SessionID: testSession}) {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if _, err := os.Stat(capture); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("process started")
		}
	}
}
func TestInteractiveLifecycle(t *testing.T) {
	for _, scenario := range []string{"success", "notification-first", "early-complete"} {
		for _, session := range []string{"", testSession} {
			t.Run(scenario+session, func(t *testing.T) {
				c := rpcClient(t, scenario)
				c.Model = "gpt-5.4"
				c.Instructions = "existing browser instructions"
				c.ReasoningEffort = "high"
				c.ServiceTier = "priority"
				c.OutputSchema = json.RawMessage(`{"type":"object"}`)
				c.MCPServers = map[string]MCPServer{"browser": {Command: "/existing/browser", Args: []string{"--profile", "existing"}}}
				t.Setenv("CODEX_HOME", "/existing/home")
				capture := filepath.Join(t.TempDir(), "capture")
				t.Setenv("CODEX_RPC_CAPTURE", capture)
				result, err := c.runInteractive(context.Background(), session, "public read only", nil)
				if err != nil || result != (Result{SessionID: testSession, Text: "interactive answer"}) {
					t.Fatalf("result=%+v err=%v", result, err)
				}
				messages := rpcCapture(t, capture)
				if len(messages) != 5 {
					t.Fatalf("messages=%s", messages)
				}
				var args []string
				_ = json.Unmarshal(messages[0]["args"], &args)
				if string(messages[0]["home"]) != `"/existing/home"` || !strings.Contains(strings.Join(args, " "), `approval_policy="on-request"`) || strings.Contains(strings.Join(args, " "), "never") {
					t.Fatalf("environment changed: %s", messages[0])
				}
				if string(messages[1]["method"]) != `"initialize"` || string(messages[2]["method"]) != `"initialized"` {
					t.Fatalf("bad handshake %s", messages)
				}
				var params map[string]json.RawMessage
				_ = json.Unmarshal(messages[3]["params"], &params)
				if string(params["developerInstructions"]) != `"existing browser instructions"` || string(params["model"]) != `"gpt-5.4"` || string(params["approvalPolicy"]) != `"on-request"` {
					t.Fatalf("thread settings: %s", params)
				}
				method := `"thread/start"`
				if session != "" {
					method = `"thread/resume"`
					if string(params["threadId"]) != `"`+session+`"` {
						t.Fatal("lost session")
					}
				}
				if string(messages[3]["method"]) != method {
					t.Fatalf("wrong thread method %s", messages[3])
				}
				_ = json.Unmarshal(messages[4]["params"], &params)
				if string(params["effort"]) != `"high"` || string(params["serviceTier"]) != `"priority"` || string(params["outputSchema"]) != `{"type":"object"}` {
					t.Fatalf("turn settings %s", params)
				}
			})
		}
	}
}
func TestInteractiveApprovals(t *testing.T) {
	for _, tc := range []struct {
		scenario string
		decision ApprovalDecision
		calls    int
		want     string
	}{
		{"command", ApprovalOnce, 1, "accept"}, {"deny", ApprovalDeny, 1, "decline"}, {"multiple", ApprovalOnce, 2, "accept"}, {"file", ApprovalOnce, 1, "accept"}, {"mcp", ApprovalOnce, 1, "accept"},
		{"invalid-decision", ApprovalDecision(99), 1, "decline"}, {"callback-error", ApprovalOnce, 1, "decline"},
		{"stale", ApprovalOnce, 0, "decline"}, {"amendment", ApprovalOnce, 0, "decline"}, {"unknown-field", ApprovalOnce, 0, "decline"}, {"stdin", ApprovalOnce, 0, "decline"}, {"grant-root", ApprovalOnce, 0, "decline"},
		{"mcp-input", ApprovalOnce, 0, "decline"}, {"mcp-persist", ApprovalOnce, 0, "decline"}, {"mcp-null-turn", ApprovalOnce, 0, "decline"}, {"mcp-url", ApprovalOnce, 0, "decline"},
	} {
		t.Run(tc.scenario, func(t *testing.T) {
			c := rpcClient(t, tc.scenario)
			capture := filepath.Join(t.TempDir(), "capture")
			t.Setenv("CODEX_RPC_CAPTURE", capture)
			requests := make(chan ApprovalRequest, 4)
			result, err := c.runInteractive(context.Background(), "", "prompt", func(ctx context.Context, req ApprovalRequest) (ApprovalDecision, error) {
				requests <- req
				if tc.scenario == "callback-error" {
					return tc.decision, errors.New("SECRET_NOT_TO_LOG")
				}
				return tc.decision, nil
			})
			if err != nil || result.Text != "interactive answer" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if len(requests) != tc.calls {
				t.Fatalf("callbacks=%d want=%d", len(requests), tc.calls)
			}
			for len(requests) > 0 {
				req := <-requests
				if req.ThreadID != testSession || req.TurnID != "turn-1" {
					t.Fatalf("uncorrelated %+v", req)
				}
				if tc.scenario == "file" && (req.FileChange == nil || req.FileChange.Changes[0].Diff != "-old\n+new") {
					t.Fatalf("missing diff %+v", req)
				}
			}
			msgs := rpcCapture(t, capture)
			for index, msg := range msgs[5:] {
				if tc.scenario == "multiple" {
					wantID := `"approval-1"`
					if index == 1 {
						wantID = `42`
					}
					if string(msg["id"]) != wantID {
						t.Fatalf("approval response ID %s, want %s", msg["id"], wantID)
					}
				}
				var body map[string]json.RawMessage
				if json.Unmarshal(msg["result"], &body) != nil {
					t.Fatalf("missing reply %s", msg)
				}
				key := "decision"
				if strings.HasPrefix(tc.scenario, "mcp") {
					key = "action"
				}
				if string(body[key]) != `"`+tc.want+`"` {
					t.Fatalf("reply %s", msg)
				}
				if tc.scenario == "mcp" && (string(body["content"]) != `{}` || string(body["_meta"]) != `null`) {
					t.Fatalf("MCP response must not persist grants: %s", msg)
				}
			}
		})
	}
}
func TestInteractiveYOLONeverPolicy(t *testing.T) {
	c := rpcClient(t, "yolo")
	c.ExecutionPolicy = ExecutionYOLO
	capture := filepath.Join(t.TempDir(), "capture")
	t.Setenv("CODEX_RPC_CAPTURE", capture)
	result, err := c.runInteractive(context.Background(), testSession, "unattended", nil)
	if err != nil || result != (Result{SessionID: testSession, Text: "interactive answer"}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	messages := rpcCapture(t, capture)
	var args []string
	_ = json.Unmarshal(messages[0]["args"], &args)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, `approval_policy="never"`) || strings.Contains(joined, "on-request") || strings.Contains(joined, "approvals_reviewer") {
		t.Fatalf("yolo must use never without a reviewer: %s", joined)
	}
	var params map[string]json.RawMessage
	_ = json.Unmarshal(messages[3]["params"], &params)
	if string(params["approvalPolicy"]) != `"never"` || string(params["approvalsReviewer"]) != "" {
		t.Fatalf("thread settings: %s", params)
	}
}

func TestInteractiveYOLOAcceptsLeftoverMCP(t *testing.T) {
	c := rpcClient(t, "yolo-mcp")
	c.ExecutionPolicy = ExecutionYOLO
	capture := filepath.Join(t.TempDir(), "capture")
	t.Setenv("CODEX_RPC_CAPTURE", capture)
	result, err := c.runInteractive(context.Background(), testSession, "unattended", nil)
	if err != nil || result.Text != "interactive answer" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	found := false
	for _, msg := range rpcCapture(t, capture) {
		if string(msg["id"]) != `"mcp-1"` {
			continue
		}
		var body map[string]json.RawMessage
		if json.Unmarshal(msg["result"], &body) != nil || string(body["action"]) != `"accept"` {
			t.Fatalf("leftover MCP not accepted: %s", msg)
		}
		found = true
	}
	if !found {
		t.Fatal("expected leftover MCP elicitation")
	}
}

func TestInteractiveFailures(t *testing.T) {
	for _, scenario := range []string{"malformed", "oversized", "stderr", "stderr-limit", "init-error", "wrong-id", "thread-error", "never", "sandbox-broadened", "reviewer-broadened", "wrong-thread", "turn-error", "failed", "stale-item", "permissions", "duplicate", "hang", "cancel", "callback-timeout", "pending-complete"} {
		t.Run(scenario, func(t *testing.T) {
			c := rpcClient(t, scenario)
			if scenario == "hang" || scenario == "callback-timeout" {
				c.Timeout = 100 * time.Millisecond
			}
			handler := func(ctx context.Context, req ApprovalRequest) (ApprovalDecision, error) {
				if scenario == "callback-timeout" || scenario == "pending-complete" {
					<-ctx.Done()
					return ApprovalOnce, nil
				}
				if scenario == "cancel" {
					return ApprovalCancel, nil
				}
				return ApprovalOnce, nil
			}
			start := time.Now()
			result, err := c.runInteractive(context.Background(), testSession, "prompt", handler)
			if err == nil || result.Text != "" || result.SessionID != testSession {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if strings.Contains(err.Error(), "SECRET_NOT_TO_LOG") {
				t.Fatalf("leaked secret: %v", err)
			}
			var ie *InteractiveError
			if !errors.As(err, &ie) {
				t.Fatalf("missing typed error: %v", err)
			}
			if (scenario == "turn-error" || scenario == "failed" || scenario == "hang") && !ie.MayHaveSideEffects {
				t.Fatal("uncertainty lost")
			}
			if scenario == "init-error" && ie.MayHaveSideEffects {
				t.Fatal("turn not submitted")
			}
			if (scenario == "hang" || scenario == "callback-timeout") && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("deadline not preserved: %v", err)
			}
			if time.Since(start) > 3*time.Second {
				t.Fatal("cleanup not bounded")
			}
		})
	}
}
func TestInteractiveNilHandler(t *testing.T) {
	c := rpcClient(t, "command")
	capture := filepath.Join(t.TempDir(), "capture")
	t.Setenv("CODEX_RPC_CAPTURE", capture)
	if _, err := c.runInteractive(context.Background(), "", "prompt", nil); err != nil {
		t.Fatal(err)
	}
	msgs := rpcCapture(t, capture)
	if string(msgs[5]["result"]) != `{"decision":"decline"}` {
		t.Fatalf("reply %s", msgs[5])
	}
}
func TestInteractiveResolvedAndConcurrent(t *testing.T) {
	for _, scenario := range []string{"resolved", "interleaved"} {
		t.Run(scenario, func(t *testing.T) {
			c := rpcClient(t, scenario)
			done := make(chan struct{})
			handler := func(ctx context.Context, req ApprovalRequest) (ApprovalDecision, error) {
				defer close(done)
				if scenario == "resolved" {
					<-ctx.Done()
				} else {
					select {
					case <-ctx.Done():
					case <-time.After(30 * time.Millisecond):
					}
				}
				return ApprovalOnce, nil
			}
			if _, err := c.runInteractive(context.Background(), "", "prompt", handler); err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("callback not canceled")
			}
		})
	}
}
func TestFreshThreadNotificationValidation(t *testing.T) {
	for _, scenario := range []string{"missing-started", "mismatch-started", "duplicate-started"} {
		t.Run(scenario, func(t *testing.T) {
			c := rpcClient(t, scenario)
			c.Timeout = 150 * time.Millisecond
			result, err := c.runInteractive(context.Background(), "", "prompt", nil)
			if err == nil || result.Text != "" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			var ie *InteractiveError
			if !errors.As(err, &ie) {
				t.Fatal(err)
			}
			if scenario != "duplicate-started" && ie.MayHaveSideEffects {
				t.Fatal("turn submitted before both thread events validated")
			}
		})
	}
}

func TestRPCIDs(t *testing.T) {
	for _, tc := range []struct {
		id    string
		valid bool
	}{{`1`, true}, {`"1"`, true}, {`null`, false}, {`true`, false}, {`1.5`, false}, {`{}`, false}, {`""`, false}} {
		if validRPCID(json.RawMessage(tc.id)) != tc.valid {
			t.Fatalf("id %s", tc.id)
		}
	}
	if reflect.DeepEqual(json.RawMessage(`1`), json.RawMessage(`"1"`)) {
		t.Fatal("IDs confused")
	}
}
