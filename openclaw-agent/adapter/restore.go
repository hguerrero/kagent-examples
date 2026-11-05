package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/kagent-dev/kagent/go/harness/runtime"
)

// Suspending an actor keeps only its volumes, so the OpenClaw gateway that
// asked for an exec approval is gone by the time kagent has the answer. The
// adapter therefore ends the OpenClaw run when a turn parks, journals the
// approval under /data, and rebuilds the pending turn from the journal after a
// restore (kagent's runtime.PendingRestorer).
//
// OpenClaw cannot continue a permission request across a restart, so the
// restored turn replays the decision to the same OpenClaw session instead. The
// session key is fixed, so OpenClaw loads the earlier transcript. An approval
// is replayed as a prompt, and the one command the user approved is allowed
// once when OpenClaw asks again. A rejection is replayed with its reason.

// parkedTurn is the journal record for one parked approval.
type parkedTurn struct {
	TaskID    string                  `json:"taskId"`
	ContextID string                  `json:"contextId"`
	Command   string                  `json:"command,omitempty"`
	Request   runtime.ApprovalRequest `json:"request"`
}

// park records a parked approval so a later process can answer it.
func (r *runner) park(p *pendingTurn) {
	if r.journal == "" {
		return
	}
	rec := parkedTurn{TaskID: p.taskID, ContextID: p.contextID, Command: p.command, Request: *p.request}
	if err := writeJournal(r.journal, rec); err != nil {
		r.logger.Warn("could not journal the parked approval; it cannot be answered after a suspend", "error", err)
	}
}

func (r *runner) clearJournal() {
	if r.journal == "" {
		return
	}
	if err := os.Remove(r.journal); err != nil && !errors.Is(err, fs.ErrNotExist) {
		r.logger.Warn("could not remove the approval journal", "error", err)
	}
}

func writeJournal(path string, rec parkedTurn) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path) // atomic: a suspend never sees half a record
}

// RestorePending returns the approval that was parked before the last suspend,
// or nothing when the actor was not waiting for one.
func (r *runner) RestorePending() (taskID, contextID string, pending runtime.PendingTurn, err error) {
	if r.journal == "" {
		return "", "", nil, nil
	}
	b, err := os.ReadFile(r.journal)
	if errors.Is(err, fs.ErrNotExist) {
		return "", "", nil, nil
	}
	if err != nil {
		return "", "", nil, fmt.Errorf("read the approval journal: %w", err)
	}
	var rec parkedTurn
	if err := json.Unmarshal(b, &rec); err != nil || rec.TaskID == "" || rec.ContextID == "" || rec.Request.ID == "" {
		// A record that cannot be answered would wedge the actor, so drop it.
		r.logger.Warn("dropping an unreadable approval journal", "error", err)
		r.clearJournal()
		return "", "", nil, nil
	}
	req := rec.Request
	return rec.TaskID, rec.ContextID, &pendingTurn{
		r: r, taskID: rec.TaskID, contextID: rec.ContextID, command: rec.Command, request: &req,
	}, nil
}

// resume answers a parked approval by replaying the decision to OpenClaw.
func (p *pendingTurn) resume(ctx context.Context, decision *runtime.ApprovalDecision, sink runtime.EventSink) (runtime.Outcome, error) {
	r := p.r
	client, sessionID, err := r.bridge.session(ctx)
	if err != nil {
		r.clearJournal()
		return failure("could not restart OpenClaw: " + err.Error()), nil
	}
	// OpenClaw re-runs a session that was running when the last gateway died.
	// Wait for that, or the replay is steered into the run and ends empty.
	r.bridge.awaitRecovery(ctx)
	id, err := client.send("session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt":    []any{map[string]any{"type": "text", "text": p.replay(decision)}},
	})
	r.clearJournal() // from here the turn runs again, and parks with a new record if it must
	if err != nil {
		r.bridge.reset(client)
		return failure("could not reach OpenClaw: " + err.Error()), nil
	}
	t := &turnState{
		client: client, sessionID: sessionID, promptID: fmt.Sprint(id), began: time.Now(), tools: map[string]string{},
		taskID: p.taskID, contextID: p.contextID,
	}
	if decision.Approved {
		t.allow = p.command
	}
	return r.pump(ctx, t, sink)
}

// replay is the prompt that tells OpenClaw what the user decided.
func (p *pendingTurn) replay(decision *runtime.ApprovalDecision) string {
	what := p.command
	if what == "" {
		what = p.request.Hint
	}
	if decision.Approved {
		return fmt.Sprintf("The user approved this exec command, but the session restarted before it ran:\n\n%s\n\nRun it now, exactly as written, and then continue with the task.", what)
	}
	reason := decision.RejectionReason
	if reason == "" {
		reason = "no reason given"
	}
	return fmt.Sprintf("The user rejected this exec command, so it did not run:\n\n%s\n\nReason: %s\n\nDo not run it. Say what you will do instead, or ask the user.", what, reason)
}
