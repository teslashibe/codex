package codex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// ReviewedInteractiveConfig is a trusted deployment review, not model or chat
// input. The reviewer must enumerate EVERY plugin in the reviewed config under
// DisabledPlugins and verify that its remaining settings contain only reviewed
// marketplace metadata and MCP definitions identical to MCPServers. In particular
// reject profiles, feature/approval overrides, hooks, extra MCP servers, custom
// instruction/config paths, and unknown settings. Hashes attest those reviewed
// bytes; this package does not interpret arbitrary TOML or infer consent.
//
// Sources maps absolute configuration-source paths to reviewed SHA-256 digests;
// empty means the path must not exist (including dangling symlinks). Required
// source paths are returned by InteractiveConfigSources. Additional deployment
// sources may be included. Existing directories are not valid source attestations.
// Review managed-preference plist contents for absence of config/requirements
// payloads before pinning their hashes. Never put authentication files here.
//
// Validation is a drift check, not protection against concurrent edits by the
// same OS user. Keep configuration stable during a run. No home or config files
// are written; explicit per-plugin disable and notify=[] overrides are ephemeral.
type ReviewedInteractiveConfig struct {
	CodexHome, WorkDir, Binary, Version string
	ConfigSHA256, MCPServersSHA256      string
	DisabledPlugins                     []string
	Sources                             map[string]string
	// Bindings pins optional adapter-owned per-run MCP servers. These names
	// must be absent from both static MCPServers and reviewed ambient config.
	Bindings map[string]ReviewedMCPBinding
}

// ReviewedMCPBinding pins a trusted adapter executable, args and fixed env.
// DynamicEnvKeys is the exact set of required socket/token fields that adapter
// may supply per run. Do not populate these definitions from chat/model output.
type ReviewedMCPBinding struct {
	Server         MCPServer
	DynamicEnvKeys []string
}

// MCPBinding supplies only per-run values, never commands or arbitrary flags.
// The adapter owns these values; they are not authorization from the model.
type MCPBinding struct {
	Name string
	Env  map[string]string
}

func (c *Client) bindInteractiveMCP(bindings []MCPBinding) (map[string]MCPServer, error) {
	servers := make(map[string]MCPServer, len(c.MCPServers)+len(bindings))
	for name, server := range c.MCPServers {
		servers[name] = server
	}
	seen := make(map[string]bool)
	for _, binding := range bindings {
		definition, ok := c.InteractiveConfig.Bindings[binding.Name]
		if !ok || seen[binding.Name] {
			return nil, configBlocked("unreviewed or duplicate per-run MCP binding")
		}
		if _, exists := servers[binding.Name]; exists {
			return nil, configBlocked("per-run MCP binding collides with static server")
		}
		seen[binding.Name] = true
		if len(binding.Env) != len(definition.DynamicEnvKeys) {
			return nil, configBlocked("per-run MCP fields do not match review")
		}
		server := definition.Server
		server.Args = slices.Clone(server.Args)
		server.Env = make(map[string]string, len(definition.Server.Env)+len(binding.Env))
		for key, value := range definition.Server.Env {
			server.Env[key] = value
		}
		keys := make(map[string]bool)
		for _, key := range definition.DynamicEnvKeys {
			value, present := binding.Env[key]
			if !present || value == "" || keys[key] {
				return nil, configBlocked("missing or duplicate per-run MCP field")
			}
			if _, fixed := server.Env[key]; fixed {
				return nil, configBlocked("per-run MCP field overwrites fixed environment")
			}
			keys[key] = true
			server.Env[key] = value
		}
		servers[binding.Name] = server
	}
	if _, err := mcpOverrides(servers); err != nil {
		return nil, configBlocked("invalid per-run MCP definition")
	}
	return servers, nil
}

// MCPServersSHA256 returns the deterministic fingerprint of explicit MCP
// definitions without disclosing their command arguments or environment values.
func MCPServersSHA256(servers map[string]MCPServer) (string, error) {
	if _, err := mcpOverrides(servers); err != nil {
		return "", err
	}
	data, err := json.Marshal(servers)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// InteractiveConfigSources enumerates known local 0.153.1/0.153.4 source paths.
// Empty/missing sources must be explicitly reviewed too: a newly created source
// then invalidates the contract. Config.toml itself is pinned by ConfigSHA256.
// Bundled system skills remain unchanged, as on exec. Disabled plugins cannot
// contribute their MCP servers or hooks. This does not enumerate remote policy;
// server-enforced requirements remain authoritative and are never weakened.
func InteractiveConfigSources(codexHome, workDir, userHome string) []string {
	paths := []string{
		filepath.Join(codexHome, "environments.toml"), filepath.Join(codexHome, "managed_config.toml"),
		"/etc/codex/config.toml", "/etc/codex/requirements.toml", "/etc/codex/managed_config.toml",
		"/Library/Preferences/com.openai.codex.plist", "/Library/Managed Preferences/com.openai.codex.plist",
		filepath.Join(userHome, "Library/Preferences/com.openai.codex.plist"),
		filepath.Join("/Library/Managed Preferences", filepath.Base(userHome), "com.openai.codex.plist"),
	}
	roots := []string{codexHome, userHome}
	for p := workDir; ; p = filepath.Dir(p) {
		roots = append(roots, p)
		if p == filepath.Dir(p) {
			break
		}
	}
	for _, root := range roots {
		for _, name := range []string{"AGENTS.md", "AGENTS.override.md", "rules", "hooks.json", ".codex/config.toml", ".codex/requirements.toml", ".codex/rules", ".codex/hooks.json", ".codex/AGENTS.md", ".codex/AGENTS.override.md", ".agents/rules"} {
			paths = append(paths, filepath.Join(root, name))
		}
	}
	slices.Sort(paths)
	return slices.Compact(paths)
}

func configBlocked(reason string) error { return &InteractiveConfigurationError{Reason: reason} }
func validDigest(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == sha256.Size && strings.ToLower(s) == s
}
func checkReviewedFile(path, digest string) error {
	info, err := os.Lstat(path)
	if digest == "" {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return configBlocked("unexpected configuration source")
	}
	if !validDigest(digest) || err != nil || !info.Mode().IsRegular() || info.Size() > maxSchema {
		return configBlocked("invalid or unavailable reviewed source")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return configBlocked("cannot read reviewed source")
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != digest {
		return configBlocked("reviewed source changed")
	}
	return nil
}

func (c *Client) validateInteractiveConfig(ctx context.Context) ([]string, error) {
	r := c.InteractiveConfig
	if r == nil {
		return nil, configBlocked("reviewed deployment contract required")
	}
	for _, p := range []string{r.CodexHome, r.WorkDir, r.Binary} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return nil, configBlocked("reviewed paths must be absolute and clean")
		}
	}
	if os.Getenv("CODEX_HOME") != r.CodexHome || c.WorkDir != r.WorkDir || c.Binary != r.Binary {
		return nil, configBlocked("home, workspace or binary changed")
	}
	if r.Version != "0.153.1" && r.Version != "0.153.4" {
		return nil, configBlocked("unsupported reviewed Codex version")
	}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "CODEX_CONFIG") || strings.HasPrefix(name, "CODEX_EXEC_SERVER") || strings.HasPrefix(name, "CODEX_PROFILE") {
			return nil, configBlocked("unreviewed configuration or execution routing environment")
		}
	}
	digest, err := MCPServersSHA256(c.MCPServers)
	if err != nil {
		return nil, err
	}
	if digest != r.MCPServersSHA256 {
		return nil, configBlocked("explicit MCP registration changed")
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return nil, configBlocked("cannot identify configuration sources")
	}
	if !filepath.IsAbs(userHome) {
		return nil, configBlocked("user home must be absolute")
	}
	required := InteractiveConfigSources(r.CodexHome, r.WorkDir, userHome)
	for _, path := range required {
		if _, ok := r.Sources[path]; !ok {
			return nil, configBlocked("configuration source missing from review")
		}
	}
	for path, digest := range r.Sources {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return nil, configBlocked("invalid reviewed source path")
		}
		if err := checkReviewedFile(path, digest); err != nil {
			return nil, err
		}
	}
	configPath := filepath.Join(r.CodexHome, "config.toml")
	if !validDigest(r.ConfigSHA256) {
		return nil, configBlocked("reviewed config fingerprint required")
	}
	if err := checkReviewedFile(configPath, r.ConfigSHA256); err != nil {
		return nil, err
	}
	plugins := slices.Clone(r.DisabledPlugins)
	slices.Sort(plugins)
	if len(plugins) > 128 {
		return nil, configBlocked("too many reviewed plugins")
	}
	args := []string{"-c", "notify=[]"}
	pluginEntries := make([]string, 0, len(plugins))
	for i, name := range plugins {
		if name == "" || len(name) > 256 || (i > 0 && plugins[i-1] == name) {
			return nil, configBlocked("invalid reviewed plugin ID")
		}
		for _, ch := range name {
			if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || strings.ContainsRune("_-.@", ch)) {
				return nil, configBlocked("invalid reviewed plugin ID")
			}
		}
		pluginEntries = append(pluginEntries, tomlString(name)+"={enabled=false}")
	}
	// CLI override paths use literal split('.'), not TOML key parsing. Quote
	// plugin IDs inside the TOML value, never in the override path. Recursive
	// merge retains marketplace metadata while setting each reviewed flag.
	args = append(args, "-c", "plugins={"+strings.Join(pluginEntries, ",")+"}")
	// Version inspection does not start app-server, MCP servers, or a model turn.
	versionCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(versionCtx, r.Binary, "--version")
	cmd.WaitDelay = waitDelay
	cleanup := configureProcessCleanup(cmd)
	defer cleanup()
	output := boundedOutput{limit: 4096, stop: cancel}
	cmd.Stdout = &output
	cmd.Stderr = &output
	if cmd.Run() != nil || output.exceeded || strings.TrimSpace(output.buf.String()) != "codex-cli "+r.Version {
		return nil, configBlocked("Codex version does not match review")
	}
	// Recheck source bytes after version execution, immediately before launch.
	if err := checkReviewedFile(configPath, r.ConfigSHA256); err != nil {
		return nil, err
	}
	for path, digest := range r.Sources {
		if err := checkReviewedFile(path, digest); err != nil {
			return nil, err
		}
	}
	return args, nil
}
