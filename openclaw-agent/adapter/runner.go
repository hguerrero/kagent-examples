package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/kagent-dev/kagent/go/harness/runtime"
)

// continuationKey is the OpenClaw session key every turn of this actor uses.
// An actor holds one conversation, so one fixed key is enough, and it is how
// the conversation survives a restart: OpenClaw keeps the transcript for the
// key in /data, and a fresh bridge started with the same key picks it up.
const continuationKey = "agent:main:kagent"

// bridge is what the runner needs from OpenClaw: a way to get a live ACP
// session, and the last model error the gateway logged.
type bridge interface {
	// session returns a client and the ACP session ID, starting both if needed.
	session(ctx context.Context) (*acpClient, string, error)
	// reset drops the client after it failed, so the next turn starts a new one.
	reset(c *acpClient)
	// modelError returns the gateway's last run error logged since the mark.
	modelError(since time.Time) string
}

// runner implements kagent's harness runtime.Runner on top of OpenClaw's ACP bridge.
type runner struct {
	logger *slog.Logger
	bridge bridge

	mu      sync.Mutex
	started bool // continuation already reported to kagent
}

// turnState is what one prompt has produced so far.
type turnState struct {
	client    *acpClient
	sessionID string
	promptID  string
	began     time.Time
	tools     map[string]string // tool call ID -> tool name
	produced  bool              // any text or tool activity
}

func (r *runner) Run(ctx context.Context, turn runtime.Turn, sink runtime.EventSink) (runtime.Outcome, error) {
	client, sessionID, err := r.bridge.session(ctx)
	if err != nil {
		return failure("could not start OpenClaw: " + err.Error()), nil
	}
	r.mu.Lock()
	first := !r.started
	r.started = true
	r.mu.Unlock()
	if first {
		if err := sink.SessionStarted(runtime.SessionStarted{ContinuationID: continuationKey}); err != nil {
			return runtime.Outcome{}, err
		}
	}
	id, err := client.send("session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt":    []any{map[string]any{"type": "text", "text": turn.Prompt}},
	})
	if err != nil {
		r.bridge.reset(client)
		return failure("could not reach OpenClaw: " + err.Error()), nil
	}
	return r.pump(ctx, &turnState{
		client: client, sessionID: sessionID, promptID: fmt.Sprint(id),
		began: time.Now(), tools: map[string]string{},
	}, sink)
}

// pump reads the bridge until the prompt finishes or OpenClaw asks for a decision.
func (r *runner) pump(ctx context.Context, t *turnState, sink runtime.EventSink) (runtime.Outcome, error) {
	for {
		m, err := t.client.next(ctx)
		switch {
		case errors.Is(err, errBridgeExited):
			r.bridge.reset(t.client)
			return failure("the OpenClaw bridge exited during the turn"), nil
		case err != nil:
			r.cancel(t)
			return runtime.Outcome{}, err
		}

		switch {
		case m.isResponse() && string(m.ID) == t.promptID:
			return r.finish(t, m), nil
		case m.Method == "session/update":
			if err := r.update(t, m.Params, sink); err != nil {
				return runtime.Outcome{}, err
			}
		case m.Method == "session/request_permission" && len(m.ID) > 0:
			t.produced = true
			pending, err := newPending(r, t, m)
			if err != nil {
				// An unreadable request must not leave OpenClaw waiting forever.
				_ = t.client.reply(m.ID, map[string]any{"outcome": map[string]any{"outcome": "cancelled"}})
				return failure("OpenClaw sent a permission request the adapter could not read: " + err.Error()), nil
			}
			return runtime.Outcome{Pending: pending}, nil
		case len(m.ID) > 0 && m.Method != "":
			// Any other request from the bridge, such as file or terminal access,
			// is one this client does not offer.
			_ = t.client.write(map[string]any{
				"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32601, "message": "method not supported"},
			})
		}
	}
}

// finish turns the prompt's final response into an outcome.
func (r *runner) finish(t *turnState, m message) runtime.Outcome {
	if m.Error != nil {
		return failure("OpenClaw rejected the prompt: " + m.Error.Message)
	}
	var res struct {
		StopReason string `json:"stopReason"`
	}
	_ = json.Unmarshal(m.Result, &res)
	switch res.StopReason {
	case "refusal":
		return failure("The model refused the request.")
	case "cancelled":
		return failure("The turn was cancelled.")
	}
	if !t.produced {
		// OpenClaw ends a turn with no output when the model call fails, and the
		// error is only in the gateway log. Surface it.
		msg := "OpenClaw returned no output."
		if detail := r.bridge.modelError(t.began); detail != "" {
			msg = "OpenClaw could not complete the turn: " + detail
		}
		return failure(msg)
	}
	return runtime.Outcome{}
}

// update maps one ACP session/update notification onto kagent's event sink.
func (r *runner) update(t *turnState, params json.RawMessage, sink runtime.EventSink) error {
	var p struct {
		Update struct {
			Kind       string          `json:"sessionUpdate"`
			Content    json.RawMessage `json:"content"`
			ToolCallID string          `json:"toolCallId"`
			Title      string          `json:"title"`
			Status     string          `json:"status"`
			RawInput   map[string]any  `json:"rawInput"`
			RawOutput  json.RawMessage `json:"rawOutput"`
		} `json:"update"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil
	}
	u := p.Update
	switch u.Kind {
	case "agent_message_chunk":
		var c struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(u.Content, &c) == nil && c.Type == "text" && c.Text != "" {
			t.produced = true
			return sink.TextDelta(runtime.TextDelta{Text: c.Text})
		}
	case "tool_call":
		if u.ToolCallID == "" {
			return nil
		}
		t.produced = true
		name := toolName(u.Title)
		t.tools[u.ToolCallID] = name
		return sink.ToolCall(runtime.ToolCall{ID: u.ToolCallID, Name: name, Arguments: u.RawInput})
	case "tool_call_update":
		// in_progress updates carry partial output; only the final one is a result.
		if u.Status != "completed" && u.Status != "failed" {
			return nil
		}
		name := t.tools[u.ToolCallID]
		if name == "" {
			name = "tool"
		}
		t.produced = true
		return sink.ToolResult(runtime.ToolResult{
			ID: u.ToolCallID, Name: name, Result: toolOutput(u.Content), IsError: u.Status == "failed",
		})
	}
	return nil
}

// toolName extracts the tool from an ACP title such as "exec: command: ls".
func toolName(title string) string {
	name, _, _ := strings.Cut(title, ": ")
	if name = strings.TrimSpace(name); name != "" {
		return name
	}
	return "tool"
}

// toolOutput flattens the text blocks of a tool_call_update.
func toolOutput(raw json.RawMessage) string {
	var blocks []struct {
		Content struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Content.Text != "" {
			parts = append(parts, b.Content.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// cancel tells OpenClaw to stop the running prompt and waits briefly for it to
// acknowledge, so the next turn does not start on a busy session.
func (r *runner) cancel(t *turnState) {
	_ = t.client.notify("session/cancel", map[string]any{"sessionId": t.sessionID})
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	for {
		m, err := t.client.next(ctx)
		if err != nil {
			r.bridge.reset(t.client)
			return
		}
		if m.isResponse() && string(m.ID) == t.promptID {
			return
		}
	}
}

func failure(msg string) runtime.Outcome {
	return runtime.Outcome{Failure: &runtime.Failure{Message: msg}}
}

// pendingTurn is a prompt parked on one of OpenClaw's exec approvals. The
// bridge process stays alive while it waits, and kagent either answers or cancels.
type pendingTurn struct {
	r       *runner
	t       *turnState
	reqID   json.RawMessage
	options []permissionOption
	request *runtime.ApprovalRequest
}

type permissionOption struct {
	OptionID string `json:"optionId"`
	Kind     string `json:"kind"`
}

func newPending(r *runner, t *turnState, m message) (*pendingTurn, error) {
	var p struct {
		ToolCall struct {
			ToolCallID string         `json:"toolCallId"`
			Title      string         `json:"title"`
			RawInput   map[string]any `json:"rawInput"`
			Meta       struct {
				ToolName   string `json:"toolName"`
				ApprovalID string `json:"approvalId"`
			} `json:"_meta"`
		} `json:"toolCall"`
		Options []permissionOption `json:"options"`
	}
	if err := json.Unmarshal(m.Params, &p); err != nil {
		return nil, err
	}
	tc := p.ToolCall
	if tc.ToolCallID == "" || len(p.Options) == 0 {
		return nil, fmt.Errorf("the request has no tool call or no options")
	}
	name := tc.Meta.ToolName
	if name == "" {
		name = toolName(tc.Title)
	}
	id := tc.Meta.ApprovalID
	if id == "" {
		id = tc.ToolCallID
	}
	hint := "OpenClaw asks to run: " + tc.Title
	if command, ok := tc.RawInput["command"].(string); ok && command != "" {
		hint = "OpenClaw asks to run the command: " + command // the title also carries timing options
	}
	return &pendingTurn{
		r: r, t: t, reqID: m.ID, options: p.Options,
		request: &runtime.ApprovalRequest{ID: id, CallID: tc.ToolCallID, Name: name, Args: tc.RawInput, Hint: hint},
	}, nil
}

func (p *pendingTurn) Request() runtime.InputRequest { return p.request }

// choose picks the option that matches the decision. ACP names them by kind;
// OpenClaw offers allow_once and reject_once.
func (p *pendingTurn) choose(approved bool) (string, bool) {
	kind := "reject_once"
	if approved {
		kind = "allow_once"
	}
	for _, o := range p.options {
		if o.Kind == kind {
			return o.OptionID, true
		}
	}
	return "", false
}

func (p *pendingTurn) Resume(ctx context.Context, response runtime.InputResponse, sink runtime.EventSink) (runtime.Outcome, error) {
	decision, ok := response.(*runtime.ApprovalDecision)
	if !ok {
		return runtime.Outcome{}, fmt.Errorf("OpenClaw only asks for approvals, got %T", response)
	}
	option, ok := p.choose(decision.Approved)
	if !ok {
		_ = p.Cancel(ctx)
		return runtime.Outcome{}, fmt.Errorf("OpenClaw offered no option that matches the decision")
	}
	if err := p.t.client.reply(p.reqID, map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": option}}); err != nil {
		p.r.bridge.reset(p.t.client)
		return failure("could not answer OpenClaw: " + err.Error()), nil
	}
	return p.r.pump(ctx, p.t, sink)
}

func (p *pendingTurn) Cancel(context.Context) error {
	_ = p.t.client.reply(p.reqID, map[string]any{"outcome": map[string]any{"outcome": "cancelled"}})
	p.r.cancel(p.t)
	return nil
}
