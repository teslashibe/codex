package codex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func digestBytes(data []byte) string { s := sha256.Sum256(data); return hex.EncodeToString(s[:]) }
func reviewedClient(t *testing.T, scenario string) *Client {
	t.Helper()
	c := rpcClient(t, scenario)
	home := t.TempDir()
	user := t.TempDir()
	t.Setenv("HOME", user)
	t.Setenv("CODEX_HOME", home)
	c.MCPServers = map[string]MCPServer{"node_repl": {Command: "/reviewed/node", Args: []string{"browser.js"}, Env: map[string]string{"PROFILE": "existing"}}, "cua_repl": {Command: "/reviewed/cua"}}
	data := []byte("notify = [\"/reviewed/notification\"]\n[plugins.\"browser@bundled\"]\nenabled = true\n[plugins.\"chrome@bundled\"]\nenabled = true\n")
	if err := os.WriteFile(filepath.Join(home, "config.toml"), data, 0600); err != nil {
		t.Fatal(err)
	}
	digest, err := MCPServersSHA256(c.MCPServers)
	if err != nil {
		t.Fatal(err)
	}
	sources := map[string]string{}
	for _, p := range InteractiveConfigSources(home, c.WorkDir, user) {
		_, err := os.Lstat(p)
		if !errors.Is(err, os.ErrNotExist) {
			t.Skip("test requires absent system/ancestor config source: " + p)
		}
		sources[p] = ""
	}
	c.InteractiveConfig = &ReviewedInteractiveConfig{CodexHome: home, WorkDir: c.WorkDir, Binary: c.Binary, Version: "0.153.1", ConfigSHA256: digestBytes(data), MCPServersSHA256: digest, DisabledPlugins: []string{"chrome@bundled", "browser@bundled"}, Sources: sources}
	return c
}
func TestReviewedInteractiveRuns(t *testing.T) {
	for _, tc := range []struct {
		scenario string
		policy   ExecutionPolicy
	}{{"success", ExecutionReadOnly}, {"workspace", ExecutionWorkspaceWrite}, {"account", ExecutionAccountAccess}} {
		t.Run(tc.scenario, func(t *testing.T) {
			c := reviewedClient(t, tc.scenario)
			c.ExecutionPolicy = tc.policy
			capture := filepath.Join(t.TempDir(), "capture")
			t.Setenv("CODEX_RPC_CAPTURE", capture)
			result, err := c.RunInteractive(context.Background(), testSession, "reviewed prompt", func(context.Context, ApprovalRequest) (ApprovalDecision, error) { return ApprovalDeny, nil })
			if err != nil || result != (Result{SessionID: testSession, Text: "interactive answer"}) {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			msgs := rpcCapture(t, capture)
			args := string(msgs[0]["args"])
			for _, want := range []string{`notify=[]`, `plugins.\"browser@bundled\".enabled=false`, `plugins.\"chrome@bundled\".enabled=false`, `node_repl`, `cua_repl`, `existing`} {
				if !strings.Contains(args, want) {
					t.Fatalf("missing %s in %s", want, args)
				}
			}
			if strings.Contains(args, "plugins={}") || strings.Contains(args, "never") {
				t.Fatalf("unsafe merge or approval override %s", args)
			}
			if string(msgs[0]["home"]) != `"`+c.InteractiveConfig.CodexHome+`"` {
				t.Fatal("home changed")
			}
			if err := checkReviewedFile(filepath.Join(c.InteractiveConfig.CodexHome, "config.toml"), c.InteractiveConfig.ConfigSHA256); err != nil {
				t.Fatal("config modified", err)
			}
		})
	}
}
func TestReviewedInteractiveDrift(t *testing.T) {
	for _, name := range []string{"config", "extra-source", "missing-source", "version", "mcp-command", "mcp-env", "plugin-injection", "plugin-duplicate", "home", "workspace", "binary", "fingerprint", "routing-env", "symlink", "new-rule-dir"} {
		t.Run(name, func(t *testing.T) {
			scenario := "success"
			if name == "version" {
				scenario = "version-drift"
			}
			c := reviewedClient(t, scenario)
			r := c.InteractiveConfig
			capture := filepath.Join(t.TempDir(), "capture")
			t.Setenv("CODEX_RPC_CAPTURE", capture)
			switch name {
			case "config":
				_ = os.WriteFile(filepath.Join(r.CodexHome, "config.toml"), []byte("changed"), 0600)
			case "extra-source":
				_ = os.WriteFile(filepath.Join(r.CodexHome, "environments.toml"), []byte("changed"), 0600)
			case "missing-source":
				delete(r.Sources, filepath.Join(r.CodexHome, "environments.toml"))
			case "version":
			case "mcp-command":
				m := c.MCPServers["node_repl"]
				m.Command = "changed"
				c.MCPServers["node_repl"] = m
			case "mcp-env":
				c.MCPServers["node_repl"].Env["PROFILE"] = "changed"
			case "plugin-injection":
				r.DisabledPlugins = []string{"bad\".enabled=true"}
			case "plugin-duplicate":
				r.DisabledPlugins = []string{"same", "same"}
			case "home":
				t.Setenv("CODEX_HOME", t.TempDir())
			case "workspace":
				c.WorkDir = t.TempDir()
			case "binary":
				c.Binary = "/different/codex"
			case "fingerprint":
				r.ConfigSHA256 = ""
			case "routing-env":
				t.Setenv("CODEX_CONFIG_FILE", "/unreviewed/config")
			case "symlink":
				p := filepath.Join(r.CodexHome, "environments.toml")
				if err := os.Symlink("/nonexistent", p); err != nil {
					t.Fatal(err)
				}
			case "new-rule-dir":
				if err := os.Mkdir(filepath.Join(r.CodexHome, "rules"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			result, err := c.RunInteractive(context.Background(), testSession, "must not run", nil)
			var blocked *InteractiveConfigurationError
			if !errors.As(err, &blocked) || result != (Result{SessionID: testSession}) {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if _, err := os.Stat(capture); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("app-server started with invalid review")
			}
		})
	}
}
func TestReviewedAdditionalSource(t *testing.T) {
	c := reviewedClient(t, "success")
	p := filepath.Join(t.TempDir(), "reviewed.plist")
	data := []byte("reviewed preference file without policy payload")
	if err := os.WriteFile(p, data, 0600); err != nil {
		t.Fatal(err)
	}
	c.InteractiveConfig.Sources[p] = digestBytes(data)
	if _, err := c.validateInteractiveConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.validateInteractiveConfig(context.Background()); err == nil {
		t.Fatal("source drift accepted")
	}
}
func TestReviewedAccountNeedsHandler(t *testing.T) {
	c := reviewedClient(t, "account")
	c.ExecutionPolicy = ExecutionAccountAccess
	if _, err := c.RunInteractive(context.Background(), testSession, "prompt", nil); !errors.Is(err, ErrInteractiveApprovalRequired) {
		t.Fatalf("err=%v", err)
	}
	// The legacy exec path must still reject account access even with a review.
	if _, err := c.Run(context.Background(), testSession, "prompt"); !errors.Is(err, ErrInteractiveApprovalRequired) {
		t.Fatalf("exec err=%v", err)
	}
}
