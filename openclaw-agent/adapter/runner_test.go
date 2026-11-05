package main

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kagent-dev/kagent/go/harness/runtime"
)

// TestMain doubles as a fake `openclaw acp`: when FAKE_OPENCLAW_ACP names a
// scenario, the test binary plays the bridge instead of running tests.
func TestMain(m *testing.M) {
	if scenario := os.Getenv("FAKE_OPENCLAW_ACP"); scenario != "" {
		fakeACP(scenario)
		return
	}
	os.Exit(m.Run())
}

func fakeACP(scenario string) {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<20)
	out := json.NewEncoder(os.Stdout)
	update := func(u map[string]any) {
		_ = out.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "s1", "update": u}})
	}
	for in.Scan() {
		var m message
		if json.Unmarshal(in.Bytes(), &m) != nil {
			continue
		}
		reply := func(result any) { _ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result}) }
		switch m.Method {
		case "initialize":
			reply(map[string]any{"protocolVersion": 1})
		case "session/new":
			reply(map[string]any{"sessionId": "s1"})
		case "session/prompt":
			switch scenario {
			case "plain":
				update(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "c1", "title": "read: path: notes.md", "status": "in_progress", "rawInput": map[string]any{"path": "notes.md"}})
				update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "c1", "status": "in_progress"})
				update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "c1", "status": "completed",
					"content": []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": "hello"}}}})
				update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "Done."}})
				reply(map[string]any{"stopReason": "end_turn"})
			case "approval":
				// A replayed rejection is just answered. Anything else asks to run
				// the command, as OpenClaw does for the first prompt and again for a
				// replayed approval.
				var ap struct {
					Prompt []struct {
						Text string `json:"text"`
					} `json:"prompt"`
				}
				_ = json.Unmarshal(m.Params, &ap)
				if len(ap.Prompt) > 0 && strings.Contains(ap.Prompt[0].Text, "rejected this exec command") {
					update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "noted"}})
					reply(map[string]any{"stopReason": "end_turn"})
					break
				}
				update(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "c1", "title": "exec: command: uname -a", "rawInput": map[string]any{"command": "uname -a"}})
				_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": 0, "method": "session/request_permission", "params": map[string]any{
					"sessionId": "s1",
					"toolCall": map[string]any{
						"toolCallId": "c1", "title": "exec: command: uname -a", "rawInput": map[string]any{"command": "uname -a"},
						"_meta": map[string]any{"toolName": "exec", "approvalId": "appr-1"},
					},
					"options": []any{
						map[string]any{"optionId": "allow-once", "name": "Allow once", "kind": "allow_once"},
						map[string]any{"optionId": "deny", "name": "Deny", "kind": "reject_once"},
					},
				}})
				// Wait for the client's answer to the permission request, then finish accordingly.
				stop := "end_turn"
				for in.Scan() {
					var a struct {
						ID     json.RawMessage `json:"id"`
						Result struct {
							Outcome struct {
								Outcome  string `json:"outcome"`
								OptionID string `json:"optionId"`
							} `json:"outcome"`
						} `json:"result"`
					}
					if json.Unmarshal(in.Bytes(), &a) != nil || string(a.ID) != "0" {
						continue
					}
					if a.Result.Outcome.Outcome == "cancelled" {
						// The adapter ended the run: the actor is about to be suspended.
						stop = "cancelled"
						break
					}
					status := "completed"
					if a.Result.Outcome.OptionID == "deny" {
						status = "failed"
					}
					update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "c1", "status": status})
					update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "answer=" + a.Result.Outcome.OptionID}})
					break
				}
				reply(map[string]any{"stopReason": stop})
			case "silent":
				reply(map[string]any{"stopReason": "end_turn"})
			case "replay":
				// A fresh OpenClaw handed the decision of an approval that was parked
				// before a suspend. It asks for the same command again when told it
				// was approved, and just answers when told it was rejected.
				var p struct {
					Prompt []struct {
						Text string `json:"text"`
					} `json:"prompt"`
				}
				_ = json.Unmarshal(m.Params, &p)
				text := ""
				if len(p.Prompt) > 0 {
					text = p.Prompt[0].Text
				}
				update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "prompt=" + text + "\n"}})
				if !strings.Contains(text, "approved") {
					reply(map[string]any{"stopReason": "end_turn"})
					break
				}
				_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": 0, "method": "session/request_permission", "params": map[string]any{
					"sessionId": "s1",
					"toolCall": map[string]any{
						"toolCallId": "c2", "title": "exec: command: uname -a", "rawInput": map[string]any{"command": "uname -a"},
						"_meta": map[string]any{"toolName": "exec", "approvalId": "appr-2"},
					},
					"options": []any{
						map[string]any{"optionId": "allow-once", "name": "Allow once", "kind": "allow_once"},
						map[string]any{"optionId": "deny", "name": "Deny", "kind": "reject_once"},
					},
				}})
				for in.Scan() {
					var a struct {
						ID     json.RawMessage `json:"id"`
						Result struct {
							Outcome struct {
								OptionID string `json:"optionId"`
							} `json:"outcome"`
						} `json:"result"`
					}
					if json.Unmarshal(in.Bytes(), &a) != nil || string(a.ID) != "0" {
						continue
					}
					update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "answer=" + a.Result.Outcome.OptionID}})
					break
				}
				reply(map[string]any{"stopReason": "end_turn"})
			}
		}
	}
}

type fakeBridge struct {
	client  *acpClient
	errs    *errorTap
	logLine string // written to the gateway log tap while the turn runs
}

func newFakeBridge(t *testing.T, scenario string) *fakeBridge {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "FAKE_OPENCLAW_ACP="+scenario)
	client, err := startACP(cmd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.call(ctx, "initialize", map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}
	return &fakeBridge{client: client, errs: &errorTap{}}
}

func (f *fakeBridge) session(context.Context) (*acpClient, string, error) { return f.client, "s1", nil }
func (f *fakeBridge) reset(*acpClient)                                    {}
func (f *fakeBridge) awaitRecovery(context.Context)                       {}
func (f *fakeBridge) modelError(since time.Time) string {
	if f.logLine != "" {
		_, _ = f.errs.Write([]byte(f.logLine))
	}
	return f.errs.since(since)
}

// recorder is an EventSink that remembers what it was given.
type recorder struct {
	text    strings.Builder
	calls   []runtime.ToolCall
	results []runtime.ToolResult
	started []string
}

func (r *recorder) SessionStarted(e runtime.SessionStarted) error {
	r.started = append(r.started, e.ContinuationID)
	return nil
}
func (r *recorder) TextDelta(e runtime.TextDelta) error { r.text.WriteString(e.Text); return nil }
func (r *recorder) ToolCall(e runtime.ToolCall) error   { r.calls = append(r.calls, e); return nil }
func (r *recorder) ToolResult(e runtime.ToolResult) error {
	r.results = append(r.results, e)
	return nil
}
func (r *recorder) ModelCall(runtime.ModelCall) {}

func newRunner(b bridge) *runner {
	return &runner{logger: slog.New(slog.NewTextHandler(os.Stderr, nil)), bridge: b}
}

func TestRunStreamsTextAndTools(t *testing.T) {
	sink := &recorder{}
	out, err := newRunner(newFakeBridge(t, "plain")).Run(context.Background(), runtime.Turn{Prompt: "hi"}, sink)
	if err != nil || out.Failure != nil || out.Pending != nil {
		t.Fatalf("outcome = %+v, err = %v", out, err)
	}
	if got := sink.text.String(); got != "Done." {
		t.Errorf("text = %q", got)
	}
	if len(sink.calls) != 1 || sink.calls[0].Name != "read" || sink.calls[0].Arguments["path"] != "notes.md" {
		t.Errorf("tool calls = %+v", sink.calls)
	}
	// The in_progress update is dropped; only the final one is a result.
	if len(sink.results) != 1 || sink.results[0].Result != "hello" || sink.results[0].IsError || sink.results[0].Name != "read" {
		t.Errorf("tool results = %+v", sink.results)
	}
	if len(sink.started) != 1 || sink.started[0] != continuationKey {
		t.Errorf("session started = %v", sink.started)
	}
}

func TestApprovalParksAndResumes(t *testing.T) {
	// The turn ends when it parks, so a decision is replayed to OpenClaw. An
	// approved command runs once, without a second approval. A rejected one does not run.
	for _, tc := range []struct {
		name        string
		approved    bool
		want        string
		wantResults int
	}{
		{"approved", true, "answer=allow-once", 1},
		{"rejected", false, "noted", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRunner(newFakeBridge(t, "approval"))
			sink := &recorder{}
			out, err := r.Run(context.Background(), runtime.Turn{Prompt: "uname"}, sink)
			if err != nil || out.Pending == nil {
				t.Fatalf("expected a pending turn, got %+v, err = %v", out, err)
			}
			req, ok := out.Pending.Request().(*runtime.ApprovalRequest)
			if ok && req.Hint != "OpenClaw asks to run the command: uname -a" {
				t.Errorf("hint = %q", req.Hint)
			}
			if !ok || req.ID != "appr-1" || req.CallID != "c1" || req.Name != "exec" || req.Args["command"] != "uname -a" {
				t.Fatalf("approval request = %+v", out.Pending.Request())
			}

			out, err = out.Pending.Resume(context.Background(), &runtime.ApprovalDecision{ID: "appr-1", Approved: tc.approved}, sink)
			if err != nil || out.Failure != nil || out.Pending != nil {
				t.Fatalf("resume outcome = %+v, err = %v", out, err)
			}
			if got := sink.text.String(); got != tc.want {
				t.Errorf("text = %q, want %q", got, tc.want)
			}
			if len(sink.results) != tc.wantResults || (tc.wantResults == 1 && sink.results[0].IsError) {
				t.Errorf("results = %+v", sink.results)
			}
		})
	}
}

func TestCancelPendingApproval(t *testing.T) {
	r := newRunner(newFakeBridge(t, "approval"))
	out, err := r.Run(context.Background(), runtime.Turn{Prompt: "uname"}, &recorder{})
	if err != nil || out.Pending == nil {
		t.Fatalf("expected a pending turn, got %+v, err = %v", out, err)
	}
	if err := out.Pending.Cancel(context.Background(), &recorder{}); err != nil {
		t.Fatal(err)
	}
}

func TestEmptyTurnReportsTheGatewayError(t *testing.T) {
	b := newFakeBridge(t, "silent")
	out, _ := newRunner(b).Run(context.Background(), runtime.Turn{Prompt: "hi"}, &recorder{})
	if out.Failure == nil || out.Failure.Message != "OpenClaw returned no output." {
		t.Fatalf("without a logged error: %+v", out)
	}

	b = newFakeBridge(t, "silent")
	b.logLine = "[agent/embedded] embedded run agent end: runId=1 isError=true model=m provider=anthropic error=request failed (HTTP 401). rawError=401: {}\n"
	out, _ = newRunner(b).Run(context.Background(), runtime.Turn{Prompt: "hi"}, &recorder{})
	if out.Failure == nil || out.Failure.Message != "OpenClaw could not complete the turn: request failed (HTTP 401)." {
		t.Fatalf("with a logged error: %+v", out)
	}
}

func TestErrorTapParsesRunErrors(t *testing.T) {
	tap := &errorTap{}
	before := time.Now().Add(-time.Second)
	line := "2026-10-08T16:33:24.064+00:00 [agent/embedded] embedded run agent end: runId=563c isError=true model=claude-sonnet-5-5 provider=anthropic error=⚠️ anthropic/claude-sonnet-5-5 request failed (authentication failed, HTTP 401). Re-authenticate the provider and try again. rawError=401: {\"error\":{}}\n"
	// A line split across writes is reassembled.
	_, _ = tap.Write([]byte(line[:40]))
	_, _ = tap.Write([]byte(line[40:]))
	want := "⚠️ anthropic/claude-sonnet-5-5 request failed (authentication failed, HTTP 401). Re-authenticate the provider and try again."
	if got := tap.since(before); got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
	// A line with no provider body still matches.
	_, _ = tap.Write([]byte("[agent/embedded] embedded run agent end: isError=true model=m error=boom\n"))
	if got := tap.since(before); got != "boom" {
		t.Errorf("error without rawError = %q", got)
	}
	if got := tap.since(time.Now().Add(time.Second)); got != "" {
		t.Errorf("an old error must not be reported for a later turn, got %q", got)
	}
}

// parkAndSuspend parks an approval on one runner and returns the journal path,
// the way an actor does just before it is suspended.
func parkAndSuspend(t *testing.T) string {
	t.Helper()
	journal := filepath.Join(t.TempDir(), "adapter", "pending.json")
	r := newRunner(newFakeBridge(t, "approval"))
	r.journal = journal
	out, err := r.Run(context.Background(), runtime.Turn{TaskID: "task-1", ContextID: "ctx-1", Prompt: "uname"}, &recorder{})
	if err != nil || out.Pending == nil {
		t.Fatalf("expected a pending turn, got %+v, err = %v", out, err)
	}
	if _, err := os.Stat(journal); err != nil {
		t.Fatalf("a parked approval must be journaled: %v", err)
	}
	return journal
}

func restoredRunner(t *testing.T, journal, scenario string) (*runner, runtime.PendingTurn) {
	t.Helper()
	r := newRunner(newFakeBridge(t, scenario))
	r.journal = journal
	taskID, contextID, pending, err := r.RestorePending()
	if err != nil || pending == nil {
		t.Fatalf("RestorePending = %q, %q, %v, %v", taskID, contextID, pending, err)
	}
	if taskID != "task-1" || contextID != "ctx-1" {
		t.Errorf("restored task = %q, context = %q", taskID, contextID)
	}
	req, ok := pending.Request().(*runtime.ApprovalRequest)
	if !ok || req.ID != "appr-1" || req.CallID != "c1" || req.Name != "exec" || req.Args["command"] != "uname -a" {
		t.Fatalf("restored request = %+v", pending.Request())
	}
	return r, pending
}

func TestRestoredApprovalRunsTheApprovedCommandWithoutAskingAgain(t *testing.T) {
	journal := parkAndSuspend(t)
	r, pending := restoredRunner(t, journal, "replay")

	sink := &recorder{}
	out, err := pending.Resume(context.Background(), &runtime.ApprovalDecision{ID: "appr-1", Approved: true}, sink)
	if err != nil || out.Failure != nil || out.Pending != nil {
		t.Fatalf("resume outcome = %+v, err = %v", out, err)
	}
	got := sink.text.String()
	if !strings.Contains(got, "approved this exec command") || !strings.Contains(got, "uname -a") {
		t.Errorf("OpenClaw was not told what the user approved: %q", got)
	}
	if !strings.Contains(got, "answer=allow-once") {
		t.Errorf("the approved command must be allowed when OpenClaw asks again: %q", got)
	}
	if _, err := os.Stat(r.journal); !os.IsNotExist(err) {
		t.Errorf("the journal must be gone once the turn runs again: %v", err)
	}
}

func TestRestoredRejectionTellsOpenClawWhy(t *testing.T) {
	journal := parkAndSuspend(t)
	_, pending := restoredRunner(t, journal, "replay")

	sink := &recorder{}
	out, err := pending.Resume(context.Background(), &runtime.ApprovalDecision{ID: "appr-1", RejectionReason: "wrong host"}, sink)
	if err != nil || out.Failure != nil || out.Pending != nil {
		t.Fatalf("resume outcome = %+v, err = %v", out, err)
	}
	got := sink.text.String()
	if !strings.Contains(got, "rejected this exec command") || !strings.Contains(got, "wrong host") {
		t.Errorf("the rejection and its reason must reach OpenClaw: %q", got)
	}
	if strings.Contains(got, "answer=") {
		t.Errorf("a rejected command must not be run: %q", got)
	}
}

func TestCancelRestoredApprovalRemovesTheJournal(t *testing.T) {
	journal := parkAndSuspend(t)
	_, pending := restoredRunner(t, journal, "replay")
	if err := pending.Cancel(context.Background(), &recorder{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(journal); !os.IsNotExist(err) {
		t.Errorf("the journal must be removed: %v", err)
	}
}

func TestRestorePendingWithNothingParkedOrABrokenJournal(t *testing.T) {
	r := newRunner(newFakeBridge(t, "plain"))
	r.journal = filepath.Join(t.TempDir(), "pending.json")
	if _, _, p, err := r.RestorePending(); p != nil || err != nil {
		t.Fatalf("no journal: pending = %v, err = %v", p, err)
	}
	if err := os.WriteFile(r.journal, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, p, err := r.RestorePending(); p != nil || err != nil {
		t.Fatalf("broken journal: pending = %v, err = %v", p, err)
	}
	if _, err := os.Stat(r.journal); !os.IsNotExist(err) {
		t.Error("a broken journal would wedge the actor, so it must be dropped")
	}
}

func TestAnsweringALiveApprovalRemovesTheJournal(t *testing.T) {
	journal := parkAndSuspend(t)
	r := newRunner(newFakeBridge(t, "approval"))
	r.journal = journal
	out, err := r.Run(context.Background(), runtime.Turn{TaskID: "task-2", ContextID: "ctx-2", Prompt: "uname"}, &recorder{})
	if err != nil || out.Pending == nil {
		t.Fatalf("pending = %+v, err = %v", out, err)
	}
	if _, err := out.Pending.Resume(context.Background(), &runtime.ApprovalDecision{ID: "appr-1", Approved: true}, &recorder{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(journal); !os.IsNotExist(err) {
		t.Errorf("the journal must be removed once the approval is answered: %v", err)
	}
}

func TestErrorTapFollowsStartupRecovery(t *testing.T) {
	tap := &errorTap{}
	if tap.recoveryPending() {
		t.Fatal("a gateway with nothing to recover logs nothing, and there is nothing to wait for")
	}
	_, _ = tap.Write([]byte("2026-11-05T17:27:50.918+00:00 [main-session-restart-recovery] marked 1 startup-orphaned main session(s) for restart recovery\n"))
	if !tap.recoveryPending() {
		t.Fatal("recovery began, but the gateway has not said how many runs it started")
	}
	_, _ = tap.Write([]byte("2026-11-05T17:27:54.357+00:00 [main-session-restart-recovery] main-session restart recovery startup complete: started=1 settled=0 failed=0 skipped=0\n"))
	if !tap.recoveryPending() {
		t.Fatal("one recovery run is still going")
	}
	// A line can arrive in two writes.
	_, _ = tap.Write([]byte("2026-11-05T17:27:58.000+00:00 [main-session-restart-recovery] main-session restart recovery term"))
	_, _ = tap.Write([]byte("inal: session=agent:main:kagent run=r1 status=ok reason=completed\n"))
	if tap.recoveryPending() {
		t.Fatal("the recovery run ended")
	}

	none := &errorTap{}
	_, _ = none.Write([]byte("[main-session-restart-recovery] main-session restart recovery startup complete: started=0 settled=0 failed=0 skipped=0\n"))
	if none.recoveryPending() {
		t.Fatal("no recovery run was started, so there is nothing to wait for")
	}
}
