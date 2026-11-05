package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	configv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/config/v1"
	"github.com/kagent-dev/kagent/go/harness/runtime"
)

// These tests drive a real OpenClaw through the real runner. They need the
// `openclaw` binary, so they run inside the image and are off by default:
//
//	go test -c -o /tmp/adapter.test ./adapter
//	docker run --rm -e OPENCLAW_INTEGRATION=1 -v /tmp/adapter.test:/adapter.test \
//	    --hostname actor --entrypoint /adapter.test <image> -test.run Integration -test.v
//
// The model is a scripted server: its first answer calls the exec tool, and
// every later answer is plain text, so a run is deterministic.

type scriptedModel struct {
	mu       sync.Mutex
	requests [][]string // the message roles of each request
	server   *httptest.Server
}

func newScriptedModel(t *testing.T) *scriptedModel {
	t.Helper()
	m := &scriptedModel{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m.server = &httptest.Server{Listener: ln, Config: &http.Server{Handler: http.HandlerFunc(m.handle)}}
	m.server.Start()
	t.Cleanup(m.server.Close)
	return m
}

func (m *scriptedModel) handle(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/embeddings") {
		// OpenClaw's memory index asks the configured provider for embeddings.
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2,0.3]}],"model":"mock-1","usage":{"prompt_tokens":1,"total_tokens":1}}`)
		return
	}
	var body struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	roles := make([]string, 0, len(body.Messages))
	hasToolResult, hasExec := false, false
	replay := -1 // the message that says the user approved a command before a restart
	for i, msg := range body.Messages {
		roles = append(roles, msg.Role)
		hasToolResult = hasToolResult || msg.Role == "tool"
		if msg.Role == "user" && strings.Contains(contentText(msg.Content), "approved this exec command") {
			replay = i
		}
	}
	if replay >= 0 {
		// A replayed approval: run the command once, unless a result already follows it.
		hasToolResult = false
		for _, msg := range body.Messages[replay+1:] {
			hasToolResult = hasToolResult || msg.Role == "tool"
		}
	}
	for _, tool := range body.Tools {
		hasExec = hasExec || tool.Function.Name == "exec"
	}
	m.mu.Lock()
	m.requests = append(m.requests, roles)
	m.mu.Unlock()

	chunk := func(delta map[string]any, finish any) string {
		b, _ := json.Marshal(map[string]any{"id": "c", "object": "chat.completion.chunk", "model": "mock-1",
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
		return "data: " + string(b) + "\n\n"
	}
	w.Header().Set("content-type", "text/event-stream")
	rejectedReplay := false
	for _, msg := range body.Messages {
		rejectedReplay = rejectedReplay || (msg.Role == "user" && strings.Contains(contentText(msg.Content), "rejected this exec command"))
	}
	if !hasToolResult && hasExec && !rejectedReplay {
		args, _ := json.Marshal(map[string]any{"command": "echo from-the-actor"})
		fmt.Fprint(w, chunk(map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
			"index": 0, "id": "call_1", "type": "function", "function": map[string]any{"name": "exec", "arguments": string(args)}}}}, nil))
		fmt.Fprint(w, chunk(map[string]any{}, "tool_calls"))
	} else {
		fmt.Fprint(w, chunk(map[string]any{"role": "assistant", "content": "The command finished."}, nil))
		fmt.Fprint(w, chunk(map[string]any{}, "stop"))
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// contentText flattens a chat message's content, which is a string or a list of parts.
func contentText(c any) string {
	switch v := c.(type) {
	case string:
		return v
	case []any:
		var sb strings.Builder
		for _, part := range v {
			if p, ok := part.(map[string]any); ok {
				if text, ok := p["text"].(string); ok {
					sb.WriteString(text)
				}
			}
		}
		return sb.String()
	}
	return ""
}

func (m *scriptedModel) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.requests)
}

// actor is one OpenClaw install over a state directory, like one boot of an actor.
type actor struct {
	bridge *openclawBridge
	runner *runner
}

func bootActor(t *testing.T, model *scriptedModel, root, execMode string) *actor {
	t.Helper()
	workspace, state, home := filepath.Join(root, "workspace"), filepath.Join(root, "openclaw"), filepath.Join(root, "home")
	configDir := t.TempDir()
	for _, d := range []string{workspace, state, home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &configv1.ADKAgentConfig{Model: &configv1.ADKModelConfig{
		Model:    "mock-1",
		Provider: &configv1.ADKModelConfig_Openai{Openai: &configv1.ADKOpenAI{BaseUrl: model.server.URL + "/v1"}},
	}}
	doc, err := buildConfig(cfg, workspace, execMode, func(k string) string { return "kagent-credential-injected" })
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := marshalConfig(doc)
	configPath := filepath.Join(configDir, "openclaw.json")
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	b, err := newBridge(logger, append(os.Environ(),
		"HOME="+home, "OPENCLAW_STATE_DIR="+state, "OPENCLAW_CONFIG_PATH="+configPath, "OPENAI_API_KEY=kagent-credential-injected"), workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.stop() })
	return &actor{bridge: b, runner: &runner{logger: logger, bridge: b, journal: filepath.Join(root, "adapter", "pending.json")}}
}

// stop kills OpenClaw without a clean shutdown, like an actor going away.
func (b *openclawBridge) stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.client != nil {
		b.client.close()
		b.client = nil
	}
	if b.gateway != nil && b.gateway.Process != nil {
		_ = b.gateway.Process.Kill()
		<-b.gwExited
	}
}

// crash kills the gateway first and the ACP bridge after it, like a suspend that
// takes the whole actor at once. Unlike stop, the gateway never sees the bridge
// leave, so it still counts the parked session as running when it comes back.
func (b *openclawBridge) crash() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.gateway != nil && b.gateway.Process != nil {
		_ = b.gateway.Process.Kill()
		<-b.gwExited
	}
	if b.client != nil {
		b.client.close()
		b.client = nil
	}
}

func integration(t *testing.T) {
	t.Helper()
	if os.Getenv("OPENCLAW_INTEGRATION") != "1" {
		t.Skip("set OPENCLAW_INTEGRATION=1 and run inside the image")
	}
}

func TestIntegrationRunsATurnWithoutApproval(t *testing.T) {
	integration(t)
	model := newScriptedModel(t)
	a := bootActor(t, model, t.TempDir(), "full")
	sink := &recorder{}
	out, err := a.runner.Run(context.Background(), runtime.Turn{Prompt: "Run a command."}, sink)
	if err != nil || out.Failure != nil || out.Pending != nil {
		t.Fatalf("outcome = %+v, err = %v", out, err)
	}
	if len(sink.calls) != 1 || sink.calls[0].Name != "exec" {
		t.Errorf("tool calls = %+v", sink.calls)
	}
	if len(sink.results) != 1 || !strings.Contains(fmt.Sprint(sink.results[0].Result), "from-the-actor") {
		t.Errorf("tool results = %+v", sink.results)
	}
	if !strings.Contains(sink.text.String(), "The command finished.") {
		t.Errorf("text = %q", sink.text.String())
	}
}

func TestIntegrationExecApprovalParksTheTurn(t *testing.T) {
	integration(t)
	for _, approved := range []bool{true, false} {
		t.Run(fmt.Sprintf("approved=%v", approved), func(t *testing.T) {
			model := newScriptedModel(t)
			a := bootActor(t, model, t.TempDir(), "ask")
			sink := &recorder{}
			out, err := a.runner.Run(context.Background(), runtime.Turn{Prompt: "Run a command."}, sink)
			if err != nil || out.Pending == nil {
				t.Fatalf("expected a pending approval, got %+v, err = %v", out, err)
			}
			req := out.Pending.Request().(*runtime.ApprovalRequest)
			if req.Name != "exec" || req.Args["command"] != "echo from-the-actor" || req.ID == "" {
				t.Fatalf("approval request = %+v", req)
			}
			out, err = out.Pending.Resume(context.Background(), &runtime.ApprovalDecision{ID: req.ID, Approved: approved}, sink)
			if err != nil || out.Failure != nil || out.Pending != nil {
				t.Fatalf("resume outcome = %+v, err = %v", out, err)
			}
			// The turn ended when it parked, so the decision was replayed: an approved
			// command runs once without asking again, and a rejected one does not run.
			if approved && (len(sink.results) != 1 || sink.results[0].IsError) {
				t.Errorf("results = %+v", sink.results)
			}
			if !approved && len(sink.results) != 0 {
				t.Errorf("a rejected command must not run: %+v", sink.results)
			}
			if !strings.Contains(sink.text.String(), "The command finished.") {
				t.Errorf("text = %q", sink.text.String())
			}
		})
	}
}

// A suspended actor loses its processes and keeps /data. The conversation must
// come back when OpenClaw starts again over the same state.
func TestIntegrationConversationSurvivesARestart(t *testing.T) {
	integration(t)
	model := newScriptedModel(t)
	root := t.TempDir()

	first := bootActor(t, model, root, "full")
	if out, err := first.runner.Run(context.Background(), runtime.Turn{Prompt: "First."}, &recorder{}); err != nil || out.Failure != nil {
		t.Fatalf("first turn: %+v, %v", out, err)
	}
	before := model.count()
	first.bridge.stop()

	second := bootActor(t, model, root, "full")
	sink := &recorder{}
	if out, err := second.runner.Run(context.Background(), runtime.Turn{Prompt: "Second."}, sink); err != nil || out.Failure != nil {
		t.Fatalf("second turn: %+v, %v", out, err)
	}
	// OpenClaw also makes small utility calls, so look at the longest request
	// the second boot sent: the turn itself, with the first conversation before it.
	model.mu.Lock()
	longest := 0
	for _, roles := range model.requests[before:] {
		longest = max(longest, len(roles))
	}
	model.mu.Unlock()
	if longest < 5 {
		t.Fatalf("the second boot did not see the first conversation: its longest request had %d messages", longest)
	}
}

// An approval parked just before a suspend is answered after the actor comes
// back: OpenClaw is a new process, so the turn is rebuilt from the journal.
func TestIntegrationApprovalSurvivesASuspend(t *testing.T) {
	integration(t)
	for _, approved := range []bool{true, false} {
		t.Run(fmt.Sprintf("approved=%v", approved), func(t *testing.T) {
			model := newScriptedModel(t)
			root := t.TempDir()

			first := bootActor(t, model, root, "ask")
			out, err := first.runner.Run(context.Background(), runtime.Turn{TaskID: "task-1", ContextID: "ctx-1", Prompt: "Run a command."}, &recorder{})
			if err != nil || out.Pending == nil {
				t.Fatalf("expected a pending approval, got %+v, err = %v", out, err)
			}
			first.bridge.crash() // the suspend: processes go, /data stays

			second := bootActor(t, model, root, "ask")
			taskID, contextID, pending, err := second.runner.RestorePending()
			if err != nil || pending == nil || taskID != "task-1" || contextID != "ctx-1" {
				t.Fatalf("RestorePending = %q, %q, %v, %v", taskID, contextID, pending, err)
			}
			req := pending.Request().(*runtime.ApprovalRequest)
			if req.Name != "exec" || req.Args["command"] != "echo from-the-actor" {
				t.Fatalf("restored request = %+v", req)
			}

			sink := &recorder{}
			out, err = pending.Resume(context.Background(), &runtime.ApprovalDecision{ID: req.ID, Approved: approved, RejectionReason: "not now"}, sink)
			if err != nil || out.Failure != nil || out.Pending != nil {
				t.Fatalf("resume outcome = %+v, err = %v", out, err)
			}
			if approved {
				if len(sink.results) != 1 || sink.results[0].IsError || !strings.Contains(fmt.Sprint(sink.results[0].Result), "from-the-actor") {
					t.Errorf("the approved command must run once, without a second approval: %+v", sink.results)
				}
			} else if len(sink.results) != 0 {
				t.Errorf("a rejected command must not run: %+v", sink.results)
			}
			if !strings.Contains(sink.text.String(), "The command finished.") {
				t.Errorf("text = %q", sink.text.String())
			}
			if _, err := os.Stat(filepath.Join(root, "adapter", "pending.json")); !os.IsNotExist(err) {
				t.Errorf("the journal must be gone: %v", err)
			}
		})
	}
}
