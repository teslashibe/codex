package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const testSession = "019cb612-9a00-7000-8000-000000000001"
const startedEvent = `{"type":"thread.started","thread_id":"` + testSession + `"}` + "\n"
const answerEvent = `{"type":"item.completed","item":{"type":"agent_message","text":"final answer"}}` + "\n"
const completedEvent = `{"type":"turn.completed","usage":{}}` + "\n"

// Re-exec the test binary as a fake CLI, avoiding shell scripts and model calls.
func TestMain(m *testing.M) {
	if scenario := os.Getenv("CODEX_RPC_TEST_HELPER"); scenario != "" {
		runRPCFixture(scenario)
		os.Exit(0)
	}
	if scenario := os.Getenv("CODEX_TEST_HELPER"); scenario != "" {
		if scenario == "descendant" {
			time.Sleep(20 * time.Second)
			os.Exit(0)
		}
		prompt, _ := io.ReadAll(os.Stdin)
		if path := os.Getenv("CODEX_TEST_CAPTURE"); path != "" {
			cwd, _ := os.Getwd()
			var schemaPath, schemaText string
			var schemaMode os.FileMode
			for i, arg := range os.Args[1:] {
				if arg == "--output-schema" {
					schemaPath = os.Args[i+2]
					data, err := os.ReadFile(schemaPath)
					if err != nil {
						panic(err)
					}
					info, err := os.Stat(schemaPath)
					if err != nil {
						panic(err)
					}
					schemaText, schemaMode = string(data), info.Mode()
				}
			}
			data, _ := json.Marshal(struct {
				Args       []string
				Prompt     string
				Dir        string
				SchemaPath string
				SchemaText string
				SchemaMode os.FileMode
				CodexHome  string
			}{os.Args[1:], string(prompt), cwd, schemaPath, schemaText, schemaMode, os.Getenv("CODEX_HOME")})
			if err := os.WriteFile(path, data, 0600); err != nil {
				panic(err)
			}
		}
		fmt.Print(startedEvent)
		switch scenario {
		case "success":
			fmt.Println(`{"type":"item.completed","item":{"type":"agent_message","text":"working..."}}`)
			fmt.Println(`{"type":"item.completed","item":{"type":"command_execution","aggregated_output":"secret tool output"}}`)
			fmt.Print(answerEvent, completedEvent)
		case "structured":
			fmt.Println(`{"type":"item.completed","item":{"type":"agent_message","text":"{\"ok\":true}"}}`)
			fmt.Print(completedEvent)
		case "exit":
			fmt.Fprint(os.Stderr, "authentication failed")
			os.Exit(3)
		case "failure":
			fmt.Println(`{"type":"turn.failed","error":{"message":"model failed"}}`)
		case "missing":
			fmt.Print(answerEvent)
		case "stdout":
			_, _ = io.Copy(os.Stdout, strings.NewReader(strings.Repeat("x", maxStdout+1)))
			time.Sleep(20 * time.Second)
		case "stderr":
			_, _ = io.Copy(os.Stderr, strings.NewReader(strings.Repeat("x", maxStderr+1)))
			time.Sleep(20 * time.Second)
		case "hang":
			time.Sleep(20 * time.Second)
		case "pipes", "child-exit":
			child := exec.Command(os.Args[0])
			child.Env = append(os.Environ(), "CODEX_TEST_HELPER=descendant")
			child.Stdout, child.Stderr = os.Stdout, os.Stderr
			if err := child.Start(); err != nil {
				panic(err)
			}
			fmt.Fprintln(os.Stderr, child.Process.Pid)
			if scenario == "child-exit" {
				fmt.Print(answerEvent, completedEvent)
				os.Exit(0)
			}
			time.Sleep(20 * time.Second)
		default:
			panic("unknown scenario: " + scenario)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeClient(t *testing.T, scenario string) *Client {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_TEST_HELPER", scenario)
	// Avoid the race runtime's default one-second sleep in each helper process.
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	return &Client{Binary: binary, WorkDir: t.TempDir(), Timeout: 5 * time.Second}
}

func TestRun(t *testing.T) {
	for _, tc := range []struct {
		name, model, effort, tier string
	}{
		{"defaults", "--dangerously-bypass-approvals-and-sandbox", "", ""},
		{"low-priority", "gpt-6-astra", "low", "priority"},
		{"effort-only", "", "low", ""},
		{"tier-only", "", "", "priority"},
		{"none-default", "", "none", "default"},
		{"minimal-flex", "", "minimal", "flex"},
		{"medium", "", "medium", ""},
		{"high", "", "high", ""},
		{"xhigh", "", "xhigh", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, session := range []string{"", testSession} {
				t.Run("session="+session, func(t *testing.T) {
					client := fakeClient(t, "success")
					client.Model = tc.model
					client.ReasoningEffort = tc.effort
					client.ServiceTier = tc.tier
					capture := t.TempDir() + "/capture.json"
					t.Setenv("CODEX_TEST_CAPTURE", capture)
					prompt := "--help\n$(touch /tmp/never-execute); 'quoted'\x00"
					result, err := client.Run(context.Background(), session, prompt)
					if err != nil || result != (Result{SessionID: testSession, Text: "final answer"}) {
						t.Fatalf("Run = %+v, %v", result, err)
					}
					var captured struct {
						Args   []string
						Prompt string
						Dir    string
					}
					data, err := os.ReadFile(capture)
					if err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(data, &captured); err != nil {
						t.Fatal(err)
					}
					want := []string{"exec", "--json", "--ignore-user-config", "--ignore-rules", "--skip-git-repo-check", "--cd", client.WorkDir,
						"-c", `sandbox_mode="read-only"`, "-c", `approval_policy="never"`}
					if tc.model != "" {
						want = append(want, "--model="+tc.model)
					}
					if tc.effort != "" {
						want = append(want, "-c", `model_reasoning_effort="`+tc.effort+`"`)
					}
					if tc.tier != "" {
						want = append(want, "-c", `service_tier="`+tc.tier+`"`)
					}
					if session == "" {
						want = append(want, "--", "-")
					} else {
						want = append(want, "resume", "--", session, "-")
					}
					if !reflect.DeepEqual(captured.Args, want) || captured.Prompt != prompt {
						t.Fatalf("captured = %+v; want args %q, prompt %q", captured, want, prompt)
					}
					actualDir, err := os.Stat(captured.Dir)
					if err != nil {
						t.Fatal(err)
					}
					wantDir, err := os.Stat(client.WorkDir)
					if err != nil || !os.SameFile(actualDir, wantDir) {
						t.Fatalf("cwd = %q; want %q (%v)", captured.Dir, client.WorkDir, err)
					}
				})
			}
		})
	}
}

func TestExecutionPolicySandboxMode(t *testing.T) {
	for _, tc := range []struct {
		policy ExecutionPolicy
		want   string
	}{
		{"", "read-only"},
		{ExecutionReadOnly, "read-only"},
		{ExecutionWorkspaceWrite, "workspace-write"},
		{ExecutionAccountAccess, "danger-full-access"},
	} {
		t.Run(string(tc.policy), func(t *testing.T) {
			got, err := tc.policy.SandboxMode()
			if err != nil || got != tc.want {
				t.Fatalf("SandboxMode = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestRunExecutionPolicy(t *testing.T) {
	for _, policy := range []ExecutionPolicy{"", ExecutionReadOnly, ExecutionWorkspaceWrite} {
		for _, session := range []string{"", testSession} {
			t.Run(string(policy)+"/session="+session, func(t *testing.T) {
				client := fakeClient(t, "success")
				client.ExecutionPolicy = policy
				capture := filepath.Join(t.TempDir(), "capture.json")
				t.Setenv("CODEX_TEST_CAPTURE", capture)
				result, err := client.Run(context.Background(), session, "prompt")
				if err != nil || result != (Result{SessionID: testSession, Text: "final answer"}) {
					t.Fatalf("Run = %+v, %v", result, err)
				}
				var captured struct{ Args []string }
				data, err := os.ReadFile(capture)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(data, &captured); err != nil {
					t.Fatal(err)
				}
				sandbox, _ := policy.SandboxMode()
				want := []string{"exec", "--json", "--ignore-user-config", "--ignore-rules", "--skip-git-repo-check", "--cd", client.WorkDir,
					"-c", "sandbox_mode=" + tomlString(sandbox), "-c", `approval_policy="never"`}
				if session == "" {
					want = append(want, "--", "-")
				} else {
					want = append(want, "resume", "--", session, "-")
				}
				if !reflect.DeepEqual(captured.Args, want) {
					t.Fatalf("args = %q; want %q", captured.Args, want)
				}
			})
		}
	}
}

func TestRunExecutionPolicyRejected(t *testing.T) {
	for _, policy := range []ExecutionPolicy{
		ExecutionAccountAccess, "danger-full-access", "unknown", "READ-ONLY", " read-only", "workspace-write ",
		"--dangerously-bypass-approvals-and-sandbox", "read-only\x00", "read-only\"\napproval_policy=\"never", "$(touch /tmp/never-execute)",
	} {
		for _, session := range []string{"", testSession} {
			t.Run(string(policy)+"/session="+session, func(t *testing.T) {
				client := fakeClient(t, "success")
				client.ExecutionPolicy = policy
				capture := filepath.Join(t.TempDir(), "capture.json")
				t.Setenv("CODEX_TEST_CAPTURE", capture)
				result, err := client.Run(context.Background(), session, "prompt")
				if err == nil || result != (Result{SessionID: session}) {
					t.Fatalf("Run = %+v, %v; want failure retaining session", result, err)
				}
				if policy == ExecutionAccountAccess {
					if !errors.Is(err, ErrInteractiveApprovalRequired) {
						t.Fatalf("missing interactive approval error: %v", err)
					}
				} else {
					if !strings.Contains(err.Error(), "invalid execution policy") {
						t.Fatalf("unexpected error: %v", err)
					}
					if mode, err := policy.SandboxMode(); err == nil || mode != "" {
						t.Fatalf("invalid policy maps to mode %q, %v", mode, err)
					}
				}
				if _, err := os.Stat(capture); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("CLI started with rejected policy: %v", err)
				}
			})
		}
	}
}

// This is an invocation regression, not evidence that a live browser executed.
// Changing execution policy must not replace sessions, the user's Codex home,
// explicit tool registration, or browser instructions.
func TestExecutionPolicyPreservesBrowserInvocation(t *testing.T) {
	for _, session := range []string{"", testSession} {
		t.Run("session="+session, func(t *testing.T) {
			client := fakeClient(t, "success")
			client.Model = "gpt-5.4"
			client.Instructions = "Use the existing browser profile; do not sign in or mutate accounts."
			client.MCPServers = map[string]MCPServer{
				"existing-browser": {Command: "/existing/browser-server", Args: []string{"--profile", "existing profile"}},
			}
			home := filepath.Join(t.TempDir(), "existing-codex-home")
			t.Setenv("CODEX_HOME", home)
			capture := filepath.Join(t.TempDir(), "capture.json")
			t.Setenv("CODEX_TEST_CAPTURE", capture)
			var baseline []string
			for _, policy := range []ExecutionPolicy{"", ExecutionReadOnly, ExecutionWorkspaceWrite} {
				client.ExecutionPolicy = policy
				result, err := client.Run(context.Background(), session, "Read a public page only.")
				if err != nil || result != (Result{SessionID: testSession, Text: "final answer"}) {
					t.Fatalf("Run = %+v, %v", result, err)
				}
				var captured struct {
					Args                   []string
					Dir, CodexHome, Prompt string
				}
				data, err := os.ReadFile(capture)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(data, &captured); err != nil {
					t.Fatal(err)
				}
				if captured.CodexHome != home || captured.Dir != client.WorkDir || captured.Prompt != "Read a public page only." {
					t.Fatalf("browser environment changed: %+v", captured)
				}
				for i, arg := range captured.Args {
					if arg == `sandbox_mode="workspace-write"` {
						captured.Args[i] = `sandbox_mode="read-only"`
					}
				}
				if baseline == nil {
					baseline = captured.Args
				} else if !reflect.DeepEqual(captured.Args, baseline) {
					t.Fatalf("policy changed browser invocation: %q; baseline %q", captured.Args, baseline)
				}
			}
		})
	}
}

func TestRunMCPServers(t *testing.T) {
	for _, session := range []string{"", testSession} {
		t.Run("session="+session, func(t *testing.T) {
			client := fakeClient(t, "success")
			client.MCPServers = map[string]MCPServer{
				"browser_1-test": {
					Command: "/usr/bin/node",
					Args:    []string{"-c", "\"\napproval_policy=\"always\"", "\\\x7f", "世界"},
					Env:     map[string]string{"TOKEN": "fake-test-secret\n\"\\\x7f", "A.b\"": "literal"},
					Cwd:     "/tmp/browser dir", StartupTimeoutSeconds: 30,
				},
				"a": {Command: "server"},
			}
			capture := filepath.Join(t.TempDir(), "capture.json")
			t.Setenv("CODEX_TEST_CAPTURE", capture)
			if _, err := client.Run(context.Background(), session, "prompt"); err != nil {
				t.Fatal(err)
			}
			var captured struct{ Args []string }
			data, err := os.ReadFile(capture)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &captured); err != nil {
				t.Fatal(err)
			}
			want := []string{"exec", "--json", "--ignore-user-config", "--ignore-rules", "--skip-git-repo-check", "--cd", client.WorkDir,
				"-c", `sandbox_mode="read-only"`, "-c", `approval_policy="never"`,
				"-c", `mcp_servers.a.command="server"`, "-c", `mcp_servers.a.enabled=true`, "-c", `mcp_servers.a.args=[]`,
				"-c", `mcp_servers.browser_1-test.command="/usr/bin/node"`, "-c", `mcp_servers.browser_1-test.enabled=true`,
				"-c", `mcp_servers.browser_1-test.args=["-c","\"\napproval_policy=\"always\"","\\\u007f","世界"]`,
				"-c", `mcp_servers.browser_1-test.env={"A.b\""="literal","TOKEN"="fake-test-secret\n\"\\\u007f"}`,
				"-c", `mcp_servers.browser_1-test.cwd="/tmp/browser dir"`, "-c", `mcp_servers.browser_1-test.startup_timeout_sec=30`,
			}
			if session == "" {
				want = append(want, "--", "-")
			} else {
				want = append(want, "resume", "--", session, "-")
			}
			if !reflect.DeepEqual(captured.Args, want) {
				t.Fatal("MCP argument capture did not match expected safe overrides")
			}
		})
	}
}

func TestMCPEnvironmentErrorRedaction(t *testing.T) {
	for _, scenario := range []string{"exit", "failure"} {
		t.Run(scenario, func(t *testing.T) {
			client := fakeClient(t, scenario)
			secret := "authentication failed"
			if scenario == "failure" {
				secret = "model failed"
			}
			client.MCPServers = map[string]MCPServer{"browser": {Command: "server", Env: map[string]string{"TOKEN": secret}}}
			_, err := client.Run(context.Background(), "", "prompt")
			if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[REDACTED]") {
				t.Fatal("MCP environment value was not redacted from error")
			}
		})
	}
}

func TestInvalidMCPServers(t *testing.T) {
	for _, tc := range []struct {
		name, serverName string
		server           MCPServer
	}{
		{"empty-name", "", MCPServer{Command: "server"}},
		{"dotted-name", "browser.env", MCPServer{Command: "server"}},
		{"option-name", "--config=x", MCPServer{Command: "server"}},
		{"unicode-name", "世界", MCPServer{Command: "server"}},
		{"missing-command", "browser", MCPServer{}},
		{"blank-command", "browser", MCPServer{Command: " \n"}},
		{"negative-timeout", "browser", MCPServer{Command: "server", StartupTimeoutSeconds: -1}},
		{"empty-env-key", "browser", MCPServer{Command: "server", Env: map[string]string{"": "secret-test-value"}}},
		{"equals-env-key", "browser", MCPServer{Command: "server", Env: map[string]string{"A=B": "secret-test-value"}}},
		{"control-env-key", "browser", MCPServer{Command: "server", Env: map[string]string{"A\nB": "secret-test-value"}}},
		{"nul-arg", "browser", MCPServer{Command: "server", Args: []string{"\x00"}}},
		{"invalid-utf8", "browser", MCPServer{Command: "server", Env: map[string]string{"TOKEN": "\xff"}}},
		{"oversized-args", "browser", MCPServer{Command: "server", Args: []string{strings.Repeat("x", 1<<20)}}},
		{"oversized-env", "browser", MCPServer{Command: "server", Env: map[string]string{"TOKEN": strings.Repeat("x", 1<<20)}}},
		{"escaped-limit", "browser", MCPServer{Command: "server", Args: []string{strings.Repeat("\x01", 200000)}}},
	} {
		for _, session := range []string{"", testSession} {
			t.Run(tc.name+"/session="+session, func(t *testing.T) {
				client := fakeClient(t, "success")
				client.MCPServers = map[string]MCPServer{tc.serverName: tc.server}
				capture := filepath.Join(t.TempDir(), "capture.json")
				t.Setenv("CODEX_TEST_CAPTURE", capture)
				result, err := client.Run(context.Background(), session, "prompt")
				if err == nil || result != (Result{SessionID: session}) {
					t.Fatal("expected MCP validation failure")
				}
				if strings.Contains(err.Error(), "secret-test-value") {
					t.Fatal("validation error disclosed environment value")
				}
				if _, err := os.Stat(capture); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("CLI started with invalid MCP configuration")
				}
			})
		}
	}
}

func TestRunInstructions(t *testing.T) {
	for _, tc := range []struct {
		name, instructions, quoted string
	}{
		{"empty", "", ""},
		{"markdown", "# Style\nUse plain text, not **markdown**.", `"# Style\nUse plain text, not **markdown**."`},
		{"injection", "\"\napproval_policy=\"always\"\n--dangerously-bypass-approvals-and-sandbox\n$(touch /tmp/never-execute); 'quoted'\\", `"\"\napproval_policy=\"always\"\n--dangerously-bypass-approvals-and-sandbox\n$(touch /tmp/never-execute); 'quoted'\\"`},
		{"controls", "\x00\a\b\t\n\v\f\r\x1f\x7f", `"\u0000\u0007\b\t\n\u000b\f\r\u001f\u007f"`},
		{"unicode", "café 世界 <>&", `"café 世界 \u003c\u003e\u0026"`},
	} {
		for _, session := range []string{"", testSession} {
			t.Run(tc.name+"/session="+session, func(t *testing.T) {
				client := fakeClient(t, "success")
				client.Instructions = tc.instructions
				capture := t.TempDir() + "/capture.json"
				t.Setenv("CODEX_TEST_CAPTURE", capture)
				result, err := client.Run(context.Background(), session, "user prompt")
				if err != nil || result != (Result{SessionID: testSession, Text: "final answer"}) {
					t.Fatalf("Run = %+v, %v", result, err)
				}
				var captured struct {
					Args   []string
					Prompt string
				}
				data, err := os.ReadFile(capture)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(data, &captured); err != nil {
					t.Fatal(err)
				}
				want := []string{"exec", "--json", "--ignore-user-config", "--ignore-rules", "--skip-git-repo-check", "--cd", client.WorkDir,
					"-c", `sandbox_mode="read-only"`, "-c", `approval_policy="never"`}
				if tc.instructions != "" {
					want = append(want, "-c", "developer_instructions="+tc.quoted)
					var decoded string
					if err := json.Unmarshal([]byte(tc.quoted), &decoded); err != nil || decoded != tc.instructions {
						t.Fatalf("instructions round trip = %q, %v", decoded, err)
					}
				}
				if session == "" {
					want = append(want, "--", "-")
				} else {
					want = append(want, "resume", "--", session, "-")
				}
				if !reflect.DeepEqual(captured.Args, want) || captured.Prompt != "user prompt" {
					t.Fatalf("captured = %+v; want args %q and unchanged prompt", captured, want)
				}
			})
		}
	}
}

func TestRunInstructionsLimit(t *testing.T) {
	for _, session := range []string{"", testSession} {
		t.Run("session="+session, func(t *testing.T) {
			client := fakeClient(t, "success")
			capture := t.TempDir() + "/capture.json"
			t.Setenv("CODEX_TEST_CAPTURE", capture)
			client.Instructions = strings.Repeat("x", maxPrompt+1)
			result, err := client.Run(context.Background(), session, "prompt")
			if err == nil || !strings.Contains(err.Error(), "instructions exceeds") || result != (Result{SessionID: session}) {
				t.Fatalf("Run = %+v, %v; want oversize instructions", result, err)
			}
			if _, err := os.Stat(capture); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("CLI started for oversize instructions: %v", err)
			}
		})
	}
}

func TestRunOutputSchema(t *testing.T) {
	for _, tc := range []struct {
		name, schema string
	}{
		{"empty", ""},
		{"object", "{}"},
		{"schema", " \n{\"type\":\"object\",\"properties\":{\"ok\":{\"type\":\"boolean\"}},\"required\":[\"ok\"],\"additionalProperties\":false}\t"},
		{"limit", "{" + strings.Repeat(" ", maxSchema-2) + "}"},
	} {
		for _, session := range []string{"", testSession} {
			t.Run(tc.name+"/session="+session, func(t *testing.T) {
				client := fakeClient(t, "structured")
				client.OutputSchema = json.RawMessage(tc.schema)
				capture := filepath.Join(t.TempDir(), "capture.json")
				t.Setenv("CODEX_TEST_CAPTURE", capture)
				result, err := client.Run(context.Background(), session, "prompt")
				if err != nil || result != (Result{SessionID: testSession, Text: `{"ok":true}`}) {
					t.Fatalf("Run = %+v, %v", result, err)
				}
				var captured struct {
					Args       []string
					SchemaPath string
					SchemaText string
					SchemaMode os.FileMode
				}
				data, err := os.ReadFile(capture)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(data, &captured); err != nil {
					t.Fatal(err)
				}
				want := []string{"exec", "--json", "--ignore-user-config", "--ignore-rules", "--skip-git-repo-check", "--cd", client.WorkDir,
					"-c", `sandbox_mode="read-only"`, "-c", `approval_policy="never"`}
				if tc.schema != "" {
					if captured.SchemaText != tc.schema || captured.SchemaMode != 0600 {
						t.Fatalf("schema contents match = %v, mode = %v", captured.SchemaText == tc.schema, captured.SchemaMode)
					}
					if !filepath.IsAbs(captured.SchemaPath) {
						t.Fatalf("schema path must be absolute: %q", captured.SchemaPath)
					}
					rel, err := filepath.Rel(client.WorkDir, captured.SchemaPath)
					if err != nil || !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
						t.Fatalf("schema must be outside work directory: %q, %v", rel, err)
					}
					if _, err := os.Stat(captured.SchemaPath); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("schema not removed: %v", err)
					}
					want = append(want, "--output-schema", captured.SchemaPath)
				}
				if session == "" {
					want = append(want, "--", "-")
				} else {
					want = append(want, "resume", "--", session, "-")
				}
				if !reflect.DeepEqual(captured.Args, want) {
					t.Fatalf("args = %q; want %q", captured.Args, want)
				}
			})
		}
	}
}

func TestRunOutputSchemaValidation(t *testing.T) {
	for _, tc := range []struct {
		name, schema, message string
	}{
		{"whitespace", " \n\t", "must be a JSON object"},
		{"null", "null", "must be a JSON object"},
		{"array", "[]", "must be a JSON object"},
		{"string", `"schema"`, "must be a JSON object"},
		{"number", "42", "must be a JSON object"},
		{"boolean", "true", "must be a JSON object"},
		{"malformed", "{", "must be a JSON object"},
		{"unicode-space", "\u00a0{}", "must be a JSON object"},
		{"trailing", "{} {}", "must be a JSON object"},
		{"oversize", "{" + strings.Repeat(" ", maxSchema-1) + "}", "output schema exceeds"},
	} {
		for _, session := range []string{"", testSession} {
			t.Run(tc.name+"/session="+session, func(t *testing.T) {
				client := fakeClient(t, "structured")
				client.OutputSchema = json.RawMessage(tc.schema)
				capture := filepath.Join(t.TempDir(), "capture.json")
				t.Setenv("CODEX_TEST_CAPTURE", capture)
				result, err := client.Run(context.Background(), session, "prompt")
				if err == nil || !strings.Contains(err.Error(), tc.message) || result != (Result{SessionID: session}) {
					t.Fatalf("Run = %+v, %v; want %q", result, err, tc.message)
				}
				if _, err := os.Stat(capture); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("CLI started for invalid schema: %v", err)
				}
			})
		}
	}
}

func TestRunOutputSchemaCleanupOnError(t *testing.T) {
	for _, scenario := range []string{"exit", "failure", "missing", "stdout", "stderr", "hang", "missing-binary", "canceled", "temp-failure"} {
		for _, session := range []string{"", testSession} {
			t.Run(scenario+"/session="+session, func(t *testing.T) {
				client := fakeClient(t, scenario)
				client.OutputSchema = json.RawMessage(`{"type":"object"}`)
				tempDir := t.TempDir()
				for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
					t.Setenv(key, tempDir)
				}
				capture := filepath.Join(t.TempDir(), "capture.json")
				t.Setenv("CODEX_TEST_CAPTURE", capture)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				switch scenario {
				case "hang":
					client.Timeout = 500 * time.Millisecond
				case "missing-binary":
					client.Binary = filepath.Join(t.TempDir(), "nonexistent")
				case "canceled":
					cancel()
				case "temp-failure":
					if err := os.Remove(tempDir); err != nil {
						t.Fatal(err)
					}
				}
				result, err := client.Run(ctx, session, "prompt")
				if err == nil || result.Text != "" {
					t.Fatalf("Run = %+v, %v; want failure with no text", result, err)
				}
				if scenario == "temp-failure" {
					if !strings.Contains(err.Error(), "create output schema") {
						t.Fatalf("unexpected error: %v", err)
					}
					return
				}
				entries, err := os.ReadDir(tempDir)
				if err != nil || len(entries) != 0 {
					t.Fatalf("schema temporary directory not empty: %v, %v", entries, err)
				}
				if scenario == "missing-binary" || scenario == "canceled" {
					return
				}
				var captured struct {
					SchemaPath string
					SchemaText string
					SchemaMode os.FileMode
				}
				data, err := os.ReadFile(capture)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(data, &captured); err != nil {
					t.Fatal(err)
				}
				if captured.SchemaPath == "" || captured.SchemaText != string(client.OutputSchema) || captured.SchemaMode != 0600 {
					t.Fatalf("schema was not readable and private during Run: %+v", captured)
				}
				if _, err := os.Stat(captured.SchemaPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("schema not removed after failure: %v", err)
				}
			})
		}
	}
}

func TestRunConfigValidation(t *testing.T) {
	for _, session := range []string{"", testSession} {
		for _, field := range []string{"reasoning effort", "service tier"} {
			for _, value := range []string{
				"unknown", "LOW", "fast", " low", "low ", "priority\n",
				"--dangerously-bypass-approvals-and-sandbox", "\x00",
				"low\"\napproval_policy=\"never", "$(touch /tmp/never-execute)",
			} {
				t.Run(field+"/"+value+"/session="+session, func(t *testing.T) {
					client := fakeClient(t, "success")
					capture := t.TempDir() + "/capture.json"
					t.Setenv("CODEX_TEST_CAPTURE", capture)
					if field == "reasoning effort" {
						client.ReasoningEffort = value
					} else {
						client.ServiceTier = value
					}
					result, err := client.Run(context.Background(), session, "prompt")
					if err == nil || !strings.Contains(err.Error(), "invalid "+field) || result != (Result{SessionID: session}) {
						t.Fatalf("Run = %+v, %v; want invalid %s", result, err, field)
					}
					if _, err := os.Stat(capture); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("CLI started for invalid configuration: %v", err)
					}
				})
			}
		}
	}
}

func TestRunFailures(t *testing.T) {
	for _, tc := range []struct{ scenario, message string }{
		{"exit", "authentication failed"},
		{"failure", "model failed"},
		{"missing", "missing thread.started or turn.completed"},
		{"stdout", "output limit exceeded"},
		{"stderr", "output limit exceeded"},
		{"child-exit", "WaitDelay"},
	} {
		t.Run(tc.scenario, func(t *testing.T) {
			client := fakeClient(t, tc.scenario)
			start := time.Now()
			result, err := client.Run(context.Background(), "", "prompt")
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("Run = %+v, %v; want %q", result, err, tc.message)
			}
			if result.SessionID != testSession || result.Text != "" {
				t.Fatalf("partial result = %+v", result)
			}
			if elapsed := time.Since(start); elapsed > 4*time.Second {
				t.Fatalf("termination took %v", elapsed)
			}
		})
	}
}

func TestCancellation(t *testing.T) {
	for _, scenario := range []string{"hang", "pipes"} {
		t.Run(scenario, func(t *testing.T) {
			client := fakeClient(t, scenario)
			client.Timeout = 500 * time.Millisecond
			start := time.Now()
			result, err := client.Run(context.Background(), testSession, "prompt")
			if !errors.Is(err, context.DeadlineExceeded) || result.SessionID != testSession || result.Text != "" {
				t.Fatalf("Run = %+v, %v", result, err)
			}
			if time.Since(start) > 3*time.Second {
				t.Fatal("cancellation blocked on inherited pipes")
			}
		})
	}
	t.Run("already canceled", func(t *testing.T) {
		client := fakeClient(t, "success")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		result, err := client.Run(ctx, testSession, "prompt")
		if !errors.Is(err, context.Canceled) || result.SessionID != testSession {
			t.Fatalf("Run = %+v, %v", result, err)
		}
	})
}

func TestValidation(t *testing.T) {
	client := &Client{Binary: "/nonexistent/codex"}
	for _, id := range []string{"--last", "--dangerously-bypass-approvals-and-sandbox", "-", "session name", "\x00", "019cb612-9a00-7000-8000-00000000000z"} {
		if _, err := client.Run(context.Background(), id, "prompt"); err == nil || !strings.Contains(err.Error(), "UUID") {
			t.Fatalf("ID %q: %v", id, err)
		}
	}
	if _, err := client.Run(context.Background(), "", strings.Repeat("p", maxPrompt+1)); err == nil || !strings.Contains(err.Error(), "prompt exceeds") {
		t.Fatalf("oversize prompt: %v", err)
	}
	client.Timeout = -time.Second
	if _, err := client.Run(context.Background(), "", "prompt"); err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("negative timeout: %v", err)
	}
	client.Timeout = 0
	if result, err := client.Run(context.Background(), testSession, "prompt"); err == nil || result.SessionID != testSession {
		t.Fatalf("missing binary = %+v, %v", result, err)
	}
}

func TestParseEvents(t *testing.T) {
	for _, tc := range []struct {
		name, events string
		wantErr      bool
	}{
		{"success", startedEvent + answerEvent + completedEvent, false},
		{"unknown events", startedEvent + "{\"type\":\"future.event\"}\n" + answerEvent + completedEvent, false},
		{"empty", "", true},
		{"malformed", startedEvent + "not json\n", true},
		{"missing type", startedEvent + "{}\n", true},
		{"recovered stream error", startedEvent + "{\"type\":\"error\",\"message\":\"retrying\"}\n" + answerEvent + completedEvent, false},
		{"missing complete", startedEvent + answerEvent, true},
		{"missing thread", answerEvent + completedEvent, true},
		{"wrong thread", strings.Replace(startedEvent, testSession, "019cb612-9a00-7000-8000-000000000002", 1) + completedEvent, true},
		{"stream error", startedEvent + "{\"type\":\"error\",\"message\":\"bad\"}\n", true},
		{"failure after complete", startedEvent + answerEvent + completedEvent + "{\"type\":\"turn.failed\"}\n", true},
		{"duplicate terminal", startedEvent + completedEvent + completedEvent, true},
		{"unfinished extra turn", startedEvent + completedEvent + "{\"type\":\"turn.started\"}\n", true},
		{"late answer", startedEvent + completedEvent + answerEvent, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := parseEvents([]byte(tc.events), testSession)
			if (err != nil) != tc.wantErr || result.SessionID != testSession {
				t.Fatalf("parse = %+v, %v", result, err)
			}
			if tc.wantErr && result.Text != "" {
				t.Fatalf("failure returned final text: %+v", result)
			}
		})
	}
}

func TestBoundedOutput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := boundedOutput{limit: 3, stop: cancel}
	if n, err := out.Write([]byte("abc")); n != 3 || err != nil || ctx.Err() != nil {
		t.Fatalf("exact limit: %d, %v, %v", n, err, ctx.Err())
	}
	if n, err := out.Write([]byte("d")); n != 0 || err == nil || ctx.Err() == nil || out.buf.String() != "abc" {
		t.Fatalf("overflow: %d, %v, %q", n, err, out.buf.String())
	}
}
