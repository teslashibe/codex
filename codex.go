// Package codex runs the official Codex CLI in a read-only sandbox.
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

const (
	maxPrompt = 1 << 20
	maxStdout = 16 << 20
	maxStderr = 64 << 10
	waitDelay = time.Second
)

// Client configures Codex invocations. Its zero value uses codex from PATH,
// the current directory, the CLI's default model, and a five-minute timeout.
// Do not modify a Client while Run is executing.
// Auth and session storage remain managed by the CLI through CODEX_HOME.
// Run limits prompts to 1 MiB, stdout to 16 MiB, and stderr to 64 KiB.
// Cancellation kills the process group on macOS/Linux; elsewhere only the CLI
// process is killed. Pipe cleanup is bounded to one additional second.
type Client struct {
	Binary  string
	WorkDir string
	Model   string
	Timeout time.Duration
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
func (c *Client) Run(ctx context.Context, sessionID, prompt string) (Result, error) {
	result := Result{SessionID: sessionID}
	if sessionID != "" && !validSessionID(sessionID) {
		return result, errors.New("codex: session ID must be a UUID")
	}
	if len(prompt) > maxPrompt {
		return result, fmt.Errorf("codex: prompt exceeds %d bytes", maxPrompt)
	}
	if c.Timeout < 0 {
		return result, errors.New("codex: timeout must not be negative")
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
	dir, err := filepath.Abs(dir)
	if err != nil {
		return result, fmt.Errorf("codex: working directory: %w", err)
	}
	args := []string{
		"exec", "--json", "--ignore-user-config", "--ignore-rules",
		"--skip-git-repo-check", "--cd", dir,
		"-c", `sandbox_mode="read-only"`, "-c", `approval_policy="never"`,
	}
	if c.Model != "" {
		args = append(args, "--model="+c.Model)
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
	cleanup := isolateProcess(cmd)
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
	if runErr != nil {
		return result, fmt.Errorf("codex: process failed: %w; stderr: %s", errors.Join(runErr, parseErr), strings.TrimSpace(stderr.buf.String()))
	}
	if parseErr != nil {
		return result, fmt.Errorf("codex: %w", parseErr)
	}
	return parsed, nil
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
