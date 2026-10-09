package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kagent-dev/kagent/go/api/adk"
)

func env(vars map[string]string) func(string) string { return func(k string) string { return vars[k] } }

func parse(t *testing.T, raw string) *adk.AgentConfig {
	t.Helper()
	var cfg adk.AgentConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	return &cfg
}

func TestAnthropicOnItsOwnEndpointNeedsNoProviderBlock(t *testing.T) {
	cfg := parse(t, `{"model":{"type":"anthropic","model":"claude-sonnet-5-5"},"instruction":"x"}`)
	doc, err := buildConfig(cfg, "/data/workspace", "", env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["models"].(map[string]any)["providers"]; ok {
		t.Error("a default endpoint must not get a provider block")
	}
	if doc["models"].(map[string]any)["catalogRefresh"].(map[string]any)["enabled"] != false {
		t.Error("the hosted catalog refresh must be off: egress would deny it")
	}
	if doc["agents"].(map[string]any)["defaults"].(map[string]any)["skipBootstrap"] != true {
		t.Error("the OpenClaw onboarding conversation must be skipped")
	}
	primary := doc["agents"].(map[string]any)["defaults"].(map[string]any)["model"].(map[string]any)["primary"]
	if primary != "anthropic/claude-sonnet-5-5" {
		t.Errorf("primary = %v", primary)
	}
	if mode := doc["tools"].(map[string]any)["exec"].(map[string]any)["mode"]; mode != "full" {
		t.Errorf("default exec mode = %v", mode)
	}
	if auth := doc["gateway"].(map[string]any)["auth"].(map[string]any); auth["token"] != nil {
		t.Error("the gateway token must come from the environment, not the file")
	}
}

func TestCustomBaseURLGetsAProviderBlockWithMaxTokens(t *testing.T) {
	cfg := parse(t, `{"model":{"type":"openai","model":"gpt-x","base_url":"https://openrouter.ai/api/v1"}}`)
	doc, err := buildConfig(cfg, "/w", "ask", env(map[string]string{"OPENAI_API_KEY": "kagent-credential-injected"}))
	if err != nil {
		t.Fatal(err)
	}
	p := doc["models"].(map[string]any)["providers"].(map[string]any)["openai"].(map[string]any)
	if p["baseUrl"] != "https://openrouter.ai/api/v1" || p["api"] != "openai-completions" || p["apiKey"] != "kagent-credential-injected" {
		t.Errorf("provider = %v", p)
	}
	if m := p["models"].([]any)[0].(map[string]any); m["maxTokens"] != 8192 {
		t.Errorf("model slot = %v", m)
	}
	if mode := doc["tools"].(map[string]any)["exec"].(map[string]any)["mode"]; mode != "ask" {
		t.Errorf("exec mode = %v", mode)
	}
}

func TestMCPServersBecomeTheRegistry(t *testing.T) {
	cfg := parse(t, `{
		"model":{"type":"anthropic","model":"m"},
		"http_tools":[
			{"params":{"url":"http://tools-gateway.agentgateway-system.svc.cluster.local/mcp","headers":{"Authorization":"__KAGENT_ENV[KAGENT_CREDENTIAL_AB]__","X-Static":"1"}},"tools":["k8s_get_resources","k8s_get_events"]},
			{"params":{"url":"http://tools-gateway.other.svc/mcp","headers":{}}}
		]}`)
	doc, err := buildConfig(cfg, "/w", "", env(map[string]string{"KAGENT_CREDENTIAL_AB": "kagent-credential-injected"}))
	if err != nil {
		t.Fatal(err)
	}
	servers := doc["mcp"].(map[string]any)["servers"].(map[string]any)
	first := servers["tools-gateway"].(map[string]any)
	if first["transport"] != "streamable-http" {
		t.Errorf("transport = %v", first["transport"])
	}
	headers := first["headers"].(map[string]string)
	if headers["Authorization"] != "kagent-credential-injected" || headers["X-Static"] != "1" {
		t.Errorf("headers = %v", headers)
	}
	if inc := first["toolFilter"].(map[string]any)["include"].([]string); len(inc) != 2 {
		t.Errorf("toolFilter = %v", first["toolFilter"])
	}
	if _, ok := servers["tools-gateway-2"]; !ok {
		t.Errorf("a second server on the same host needs a distinct name, got %v", servers)
	}
}

func TestUnsupportedInputsAreRefusedLoudly(t *testing.T) {
	for name, tc := range map[string]struct{ raw, mode, want string }{
		"no model":       {`{}`, "", "needs a modelConfig"},
		"bedrock":        {`{"model":{"type":"bedrock","model":"m"}}`, "", "not supported"},
		"bad exec mode":  {`{"model":{"type":"anthropic","model":"m"}}`, "yolo", "OPENCLAW_EXEC_MODE"},
		"mcp approval":   {`{"model":{"type":"anthropic","model":"m"},"http_tools":[{"params":{"url":"http://x/mcp"},"require_approval":true}]}`, "", "requireApproval"},
		"missing secret": {`{"model":{"type":"anthropic","model":"m"},"http_tools":[{"params":{"url":"http://x/mcp","headers":{"A":"__KAGENT_ENV[NOPE]__"}}}]}`, "", "NOPE"},
		"stdio tool":     {`{"model":{"type":"anthropic","model":"m"},"stdio_tools":[{"command":"x"}]}`, "", "not supported"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := buildConfig(parse(t, tc.raw), "/w", tc.mode, env(nil))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestInstructionsAreMarkedAsManaged(t *testing.T) {
	if got := instructions(&adk.AgentConfig{}); got != "" {
		t.Errorf("no instruction should write no file, got %q", got)
	}
	got := instructions(&adk.AgentConfig{Instruction: "  Be brief.  "})
	if !strings.Contains(got, "Managed by kagent") || !strings.HasSuffix(got, "Be brief.\n") {
		t.Errorf("instructions = %q", got)
	}
}
