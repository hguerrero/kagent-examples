package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	configv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/config/v1"
)

const (
	gatewayPort = 18789
	execModeEnv = "OPENCLAW_EXEC_MODE"
)

// execModes are the values OpenClaw accepts for tools.exec.mode.
var execModes = map[string]bool{"full": true, "auto": true, "ask": true, "allowlist": true, "deny": true}

// buildConfig turns the configuration kagent compiled from the AgentTemplate
// into an openclaw.json document. getenv resolves the credential placeholders
// kagent leaves in MCP headers.
func buildConfig(cfg *configv1.ADKAgentConfig, workspace, execMode string, getenv func(string) string) (map[string]any, error) {
	if cfg.GetModel() == nil {
		return nil, fmt.Errorf("the AgentTemplate needs a modelConfig: OpenClaw has no default model")
	}
	primary, providers, err := translateModel(cfg.GetModel(), getenv)
	if err != nil {
		return nil, err
	}
	servers, err := translateMCP(cfg, getenv)
	if err != nil {
		return nil, err
	}
	if len(cfg.GetStdioTools()) > 0 || len(cfg.GetRemoteAgents()) > 0 || len(cfg.GetSubAgents()) > 0 {
		return nil, fmt.Errorf("stdio tools, remote agents and sub-agents are not supported by this adapter")
	}
	if execMode == "" {
		execMode = "full"
	}
	if !execModes[execMode] {
		return nil, fmt.Errorf("%s=%q is not one of full, auto, ask, allowlist, deny", execModeEnv, execMode)
	}

	out := map[string]any{
		"gateway": map[string]any{
			"mode": "local", "bind": "loopback", "port": gatewayPort,
			// The token comes from OPENCLAW_GATEWAY_TOKEN, so it is never written to disk.
			"auth": map[string]any{"mode": "token"},
		},
		"agents": map[string]any{"defaults": map[string]any{
			"workspace": workspace,
			"model":     map[string]any{"primary": primary},
			// Without this, the first message starts OpenClaw's onboarding: it
			// asks the user to name the agent. A platform-managed agent has its
			// persona in the AgentTemplate instead.
			"skipBootstrap": true,
		}},
		"tools": map[string]any{"exec": map[string]any{"mode": execMode}},
		// Default-deny egress would block these calls anyway, so turn them off
		// instead of logging a 403 for each. They are the update check, and the
		// hosted model catalog refresh.
		"update": map[string]any{"checkOnStart": false},
		"models": map[string]any{"catalogRefresh": map[string]any{"enabled": false}},
	}
	if len(providers) > 0 {
		out["models"].(map[string]any)["providers"] = providers
	}
	if len(servers) > 0 {
		out["mcp"] = map[string]any{"servers": servers}
	}
	return out, nil
}

// translateModel maps a kagent model onto an OpenClaw model reference. A model
// on its provider's own endpoint needs nothing else: OpenClaw reads the API key
// from the environment. A custom base URL needs a provider block.
func translateModel(model *configv1.ADKModelConfig, getenv func(string) string) (string, map[string]any, error) {
	switch p := model.GetProvider().(type) {
	case *configv1.ADKModelConfig_Anthropic:
		primary := "anthropic/" + model.GetModel()
		baseURL := p.Anthropic.GetBaseUrl()
		if baseURL == "" {
			return primary, nil, nil
		}
		maxTokens := 8192
		if p.Anthropic.MaxTokens != nil {
			maxTokens = int(*p.Anthropic.MaxTokens)
		}
		return primary, map[string]any{"anthropic": customProvider(baseURL, "anthropic-messages", getenv("ANTHROPIC_API_KEY"), model.GetModel(), maxTokens)}, nil
	case *configv1.ADKModelConfig_Openai:
		primary := "openai/" + model.GetModel()
		baseURL := p.Openai.GetBaseUrl()
		if baseURL == "" || strings.HasPrefix(baseURL, "https://api.openai.com") {
			return primary, nil, nil
		}
		api := "openai-completions"
		if p.Openai.GetApiFormat() == "responses" {
			api = "openai-responses"
		}
		maxTokens := 8192
		if p.Openai.MaxTokens != nil {
			maxTokens = int(*p.Openai.MaxTokens)
		}
		return primary, map[string]any{"openai": customProvider(baseURL, api, getenv("OPENAI_API_KEY"), model.GetModel(), maxTokens)}, nil
	default:
		return "", nil, fmt.Errorf("model provider %T is not supported: use an Anthropic or OpenAI ModelConfig", p)
	}
}

// customProvider is the block OpenClaw needs for an endpoint it does not know.
// Anthropic's transport refuses a model slot without maxTokens, so it is always set.
func customProvider(baseURL, api, apiKey, model string, maxTokens int) map[string]any {
	return map[string]any{
		"baseUrl": baseURL, "api": api, "apiKey": apiKey,
		"models": []any{map[string]any{"id": model, "name": model, "contextWindow": 200000, "maxTokens": maxTokens}},
	}
}

// translateMCP maps the MCP servers bound to the AgentTemplate onto OpenClaw's
// own MCP client registry. The template's tool list becomes a tool filter.
func translateMCP(cfg *configv1.ADKAgentConfig, getenv func(string) string) (map[string]any, error) {
	servers := map[string]any{}
	add := func(rawURL, transport string, headers map[string]string, tools []string, requireApproval, tokenPassthrough bool) error {
		if tokenPassthrough {
			return fmt.Errorf("tokenPassthrough on MCP server %s is not supported by this adapter", rawURL)
		}
		if requireApproval {
			// OpenClaw gates MCP tool calls only in its Codex runtime. Refuse
			// instead of running a tool the template says must be approved.
			return fmt.Errorf("requireApproval on MCP server %s is not supported: OpenClaw approves exec commands, not MCP tool calls", rawURL)
		}
		expanded, err := expandHeaders(headers, getenv)
		if err != nil {
			return err
		}
		entry := map[string]any{"url": rawURL, "transport": transport, "enabled": true}
		if len(expanded) > 0 {
			entry["headers"] = expanded
		}
		if len(tools) > 0 {
			entry["toolFilter"] = map[string]any{"include": tools}
		}
		servers[serverName(rawURL, servers)] = entry
		return nil
	}
	for _, t := range cfg.GetHttpTools() {
		if err := add(t.GetParams().GetUrl(), "streamable-http", t.GetParams().GetHeaders(), t.GetTools(), t.GetRequireApproval(), t.GetTokenPassthrough()); err != nil {
			return nil, err
		}
	}
	for _, t := range cfg.GetSseTools() {
		if err := add(t.GetParams().GetUrl(), "sse", t.GetParams().GetHeaders(), t.GetTools(), t.GetRequireApproval(), t.GetTokenPassthrough()); err != nil {
			return nil, err
		}
	}
	return servers, nil
}

var nonName = regexp.MustCompile(`[^a-z0-9]+`)

// serverName derives a registry name from the server's host, because the
// compiled configuration does not carry the RemoteMCPServer name. OpenClaw
// prefixes every tool with it, as in kubernetes__k8s_get_resources.
func serverName(rawURL string, taken map[string]any) string {
	name := "mcp"
	if u, err := url.Parse(rawURL); err == nil && u.Hostname() != "" {
		name = nonName.ReplaceAllString(strings.ToLower(strings.SplitN(u.Hostname(), ".", 2)[0]), "-")
		name = strings.Trim(name, "-")
	}
	if name == "" {
		name = "mcp"
	}
	candidate := name
	for i := 2; ; i++ {
		if _, exists := taken[candidate]; !exists {
			return candidate
		}
		candidate = fmt.Sprintf("%s-%d", name, i)
	}
}

const envPrefix, envSuffix = "__KAGENT_ENV[", "]__"

// expandHeaders resolves the __KAGENT_ENV[NAME]__ markers kagent uses for
// header values that come from a Secret. The environment holds the inert
// placeholder, so the expanded value is not a secret.
func expandHeaders(headers map[string]string, getenv func(string) string) (map[string]string, error) {
	out := make(map[string]string, len(headers))
	for name, value := range headers {
		if strings.HasPrefix(value, envPrefix) && strings.HasSuffix(value, envSuffix) {
			key := strings.TrimSuffix(strings.TrimPrefix(value, envPrefix), envSuffix)
			if value = getenv(key); value == "" {
				return nil, fmt.Errorf("header %s needs environment variable %s, which is not set", name, key)
			}
		}
		out[name] = value
	}
	return out, nil
}

// instructions is the AGENTS.md kagent manages. OpenClaw loads it at the start
// of every session, and it replaced the removed systemPromptOverride setting.
func instructions(cfg *configv1.ADKAgentConfig) string {
	if strings.TrimSpace(cfg.GetInstruction()) == "" {
		return ""
	}
	return "<!-- Managed by kagent from the AgentTemplate system prompt. Edits are overwritten when the actor starts. -->\n\n" +
		strings.TrimSpace(cfg.GetInstruction()) + "\n"
}

func marshalConfig(doc map[string]any) ([]byte, error) {
	return json.MarshalIndent(doc, "", "  ")
}
