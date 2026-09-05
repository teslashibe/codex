package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
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
	if scenario := os.Getenv("CODEX_TEST_HELPER"); scenario != "" {
		if scenario == "descendant" {
			time.Sleep(20 * time.Second)
			os.Exit(0)
		}
		prompt, _ := io.ReadAll(os.Stdin)
		if path := os.Getenv("CODEX_TEST_CAPTURE"); path != "" {
			cwd, _ := os.Getwd()
			data, _ := json.Marshal(struct {
				Args   []string
				Prompt string
				Dir    string
			}{os.Args[1:], string(prompt), cwd})
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
	for _, session := range []string{"", testSession} {
		t.Run("session="+session, func(t *testing.T) {
			client := fakeClient(t, "success")
			client.Model = "--dangerously-bypass-approvals-and-sandbox"
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
				"-c", `sandbox_mode="read-only"`, "-c", `approval_policy="never"`, "--model=" + client.Model}
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
