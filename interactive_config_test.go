package codex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
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
			for _, want := range []string{`notify=[]`, `\"browser@bundled\"={enabled=false}`, `\"chrome@bundled\"={enabled=false}`, `node_repl`, `cua_repl`, `existing`} {
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
func TestPluginOverrideActualTOMLSemantics(t *testing.T) {
	c := reviewedClient(t, "success")
	c.InteractiveConfig.DisabledPlugins = []string{"browser@bundled", "chrome.v2@market.place"}
	args, err := c.validateInteractiveConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(args)
	// Python's standard TOML parser validates the value syntax; path splitting
	// and recursive merge mirror Codex 0.153.1 config/overrides.rs and merge.rs.
	// No external service, model, app-server, plugin or tool is started.
	cmd := exec.Command("python3", "-c", `import json,sys,tomllib,copy
args=json.load(sys.stdin)
base={'plugins':{'browser@bundled':{'enabled':True,'review':'keep'},'chrome.v2@market.place':{'enabled':True}},'marketplaces':{'bundled':{'source':'unchanged'}},'mcp_servers':{'node_repl':{'command':'unchanged'},'cua_repl':{'command':'unchanged'}}}
original=copy.deepcopy(base)
layer={}
for i in range(0,len(args),2):
 assert args[i]=='-c'
 path,value=args[i+1].split('=',1)
 value=tomllib.loads('value='+value)['value']
 parts=path.split('.')
 target=layer
 for part in parts[:-1]: target=target.setdefault(part,{})
 target[parts[-1]]=value
def merge(a,b):
 for k,v in b.items():
  if isinstance(v,dict) and isinstance(a.get(k),dict): merge(a[k],v)
  else:a[k]=v
merge(base,layer)
assert set(base['plugins'])==set(original['plugins'])
assert all(not p['enabled'] for p in base['plugins'].values())
assert base['plugins']['browser@bundled']['review']=='keep'
assert base['marketplaces']==original['marketplaces']
assert base['mcp_servers']==original['mcp_servers']
assert base['notify']==[]
`)
	cmd.Stdin = bytes.NewReader(payload)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("upstream-equivalent override semantics: %v %s", err, out)
	}
}

func TestReviewedDynamicBindings(t *testing.T) {
	for _, session := range []string{"", testSession} {
		t.Run("session="+session, func(t *testing.T) {
			c := reviewedClient(t, "success")
			c.InteractiveConfig.Bindings = map[string]ReviewedMCPBinding{"authorized_notes": {Server: MCPServer{Command: "/reviewed/notes-adapter", Args: []string{"serve"}, Env: map[string]string{"MODE": "reviewed"}}, DynamicEnvKeys: []string{"SOCKET", "TOKEN"}}}
			staticHash, _ := MCPServersSHA256(c.MCPServers)
			for _, socket := range []string{"/tmp/notes-one.sock", "/tmp/notes-two.sock"} {
				capture := filepath.Join(t.TempDir(), "capture")
				t.Setenv("CODEX_RPC_CAPTURE", capture)
				binding := MCPBinding{Name: "authorized_notes", Env: map[string]string{"SOCKET": socket, "TOKEN": "per-run-secret"}}
				result, err := c.RunInteractive(context.Background(), session, "prompt", nil, binding)
				if err != nil || result.SessionID != testSession {
					t.Fatalf("result=%+v err=%v", result, err)
				}
				msgs := rpcCapture(t, capture)
				args := string(msgs[0]["args"])
				if !strings.Contains(args, socket) || !strings.Contains(args, "authorized_notes") || !strings.Contains(args, "node_repl") || !strings.Contains(args, "cua_repl") {
					t.Fatalf("incorrect merged registration %s", args)
				}
				got, _ := MCPServersSHA256(c.MCPServers)
				if got != staticHash {
					t.Fatal("static map mutated")
				}
			}
			c.MCPServers["node_repl"].Env["PROFILE"] = "drift"
			if _, err := c.RunInteractive(context.Background(), session, "prompt", nil, MCPBinding{Name: "authorized_notes", Env: map[string]string{"SOCKET": "/tmp/new", "TOKEN": "new"}}); err == nil {
				t.Fatal("static drift blessed by dynamic binding")
			}
		})
	}
}

func TestBindingRejectsBroadening(t *testing.T) {
	for _, kind := range []string{"extra-server", "collision", "extra-env", "missing-env", "fixed-env", "duplicate", "duplicate-key"} {
		t.Run(kind, func(t *testing.T) {
			c := reviewedClient(t, "success")
			def := ReviewedMCPBinding{Server: MCPServer{Command: "/reviewed/adapter", Args: []string{"serve"}, Env: map[string]string{"FIXED": "pinned"}}, DynamicEnvKeys: []string{"SOCKET"}}
			c.InteractiveConfig.Bindings = map[string]ReviewedMCPBinding{"authorized_notes": def}
			bindings := []MCPBinding{{Name: "authorized_notes", Env: map[string]string{"SOCKET": "/tmp/one"}}}
			switch kind {
			case "extra-server":
				bindings[0].Name = "other"
			case "collision":
				bindings[0].Name = "node_repl"
				c.InteractiveConfig.Bindings["node_repl"] = def
			case "extra-env":
				bindings[0].Env["UNREVIEWED"] = "value"
			case "missing-env":
				bindings[0].Env = map[string]string{}
			case "fixed-env":
				def.DynamicEnvKeys = []string{"FIXED"}
				c.InteractiveConfig.Bindings["authorized_notes"] = def
				bindings[0].Env = map[string]string{"FIXED": "overwrite"}
			case "duplicate":
				bindings = append(bindings, bindings[0])
			case "duplicate-key":
				def.DynamicEnvKeys = []string{"SOCKET", "SOCKET"}
				c.InteractiveConfig.Bindings["authorized_notes"] = def
			}
			capture := filepath.Join(t.TempDir(), "capture")
			t.Setenv("CODEX_RPC_CAPTURE", capture)
			if _, err := c.RunInteractive(context.Background(), testSession, "prompt", nil, bindings...); err == nil {
				t.Fatal("unreviewed binding accepted")
			}
			if _, err := os.Stat(capture); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("app-server started")
			}
		})
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

func TestReviewedYOLORunsWithoutHandler(t *testing.T) {
	c := reviewedClient(t, "yolo")
	c.ExecutionPolicy = ExecutionYOLO
	capture := filepath.Join(t.TempDir(), "capture")
	t.Setenv("CODEX_RPC_CAPTURE", capture)
	result, err := c.RunInteractive(context.Background(), testSession, "reviewed prompt", nil)
	if err != nil || result != (Result{SessionID: testSession, Text: "interactive answer"}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	var args []string
	_ = json.Unmarshal(rpcCapture(t, capture)[0]["args"], &args)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, `approval_policy="never"`) || strings.Contains(joined, "on-request") {
		t.Fatalf("reviewed yolo args: %s", joined)
	}
}
