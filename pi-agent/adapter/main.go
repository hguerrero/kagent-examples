// Command pi-a2a-adapter exposes the Pi coding agent to kagent over A2A.
//
// kagent talks A2A over gRPC on port 80 inside the actor. This adapter starts
// `pi --mode rpc` as a child process, turns each A2A message into a Pi `prompt`
// command, streams Pi's text back as it arrives, and reports the turn finished
// when Pi emits `agent_settled`.
//
// An actor is cold-booted after every suspend, so the adapter process (and the
// Pi process under it) starts fresh on each resume. To keep the conversation,
// the adapter records Pi's session file on the durable volume after each turn
// and switches Pi back to it on the next start.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/kagent-dev/kagent/go/adk/pkg/app"
	apia2a "github.com/kagent-dev/kagent/go/api/a2a"
	"github.com/kagent-dev/kagent/go/pkg/logging"
)

const (
	agentCardEnv = "KAGENT_AGENT_CARD_JSON" // rendered by kagent from the AgentTemplate
	dataDir      = "/data"                  // durable directory in the actor
	privatePort  = "80"                     // fixed by the kagent A2A contract
	piStartup    = 90 * time.Second         // time allowed for Pi to answer its first command
)

// piEvent is the subset of Pi's RPC JSONL output that the adapter needs.
type piEvent struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Command string `json:"command"`
	Success *bool  `json:"success"`
	Error   string `json:"error"`
	Data    struct {
		SessionFile string `json:"sessionFile"`
		Cancelled   bool   `json:"cancelled"`
	} `json:"data"`
	AssistantMessageEvent struct {
		Type  string `json:"type"`
		Delta string `json:"delta"`
	} `json:"assistantMessageEvent"`
}

// piProcess owns one long-lived `pi --mode rpc` child process.
type piProcess struct {
	stdin  io.WriteCloser
	events chan piEvent
	seq    atomic.Int64
}

func (p *piProcess) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = p.stdin.Write(append(b, '\n'))
	return err
}

// request sends a command and waits for its response, skipping other events.
// Call it only while no turn is running.
func (p *piProcess) request(ctx context.Context, command map[string]any) (piEvent, error) {
	id := fmt.Sprintf("adapter-%d", p.seq.Add(1))
	command["id"] = id
	if err := p.send(command); err != nil {
		return piEvent{}, err
	}
	for {
		select {
		case <-ctx.Done():
			return piEvent{}, ctx.Err()
		case ev, ok := <-p.events:
			if !ok {
				return piEvent{}, fmt.Errorf("pi exited")
			}
			if ev.Type != "response" || ev.ID != id {
				continue
			}
			if ev.Success != nil && !*ev.Success {
				return ev, fmt.Errorf("pi rejected %v: %s", command["type"], ev.Error)
			}
			return ev, nil
		}
	}
}

func sessionPointer() string { return filepath.Join(dataDir, "adapter", "pi-session") }

func startPi(ctx context.Context, logger *slog.Logger) (*piProcess, error) {
	// The durable volume is mounted over /data and starts empty, so nothing
	// created in the image under /data exists yet. Create what Pi needs.
	workspace := filepath.Join(dataDir, "workspace")
	sessionDir := filepath.Join(dataDir, "pi-agent", "sessions")
	for _, dir := range []string{workspace, sessionDir, filepath.Dir(sessionPointer())} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}
	args := []string{"--mode", "rpc", "--session-dir", sessionDir}
	if provider := os.Getenv("PI_PROVIDER"); provider != "" {
		args = append(args, "--provider", provider)
	}
	if model := os.Getenv("PI_MODEL"); model != "" {
		args = append(args, "--model", model)
	}
	cmd := exec.Command("pi", args...)
	cmd.Dir = workspace
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &piProcess{stdin: stdin, events: make(chan piEvent, 256)}
	go func() {
		defer close(p.events)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1024*1024), 16*1024*1024) // Pi can emit large lines
		for sc.Scan() {
			var ev piEvent
			if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
				logger.Warn("skipping non-JSON line from pi", "error", err)
				continue
			}
			p.events <- ev
		}
	}()

	// Wait until Pi answers a command, so the first prompt never races startup.
	ready, cancel := context.WithTimeout(ctx, piStartup)
	defer cancel()
	if _, err := p.request(ready, map[string]any{"type": "get_state"}); err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("pi did not become ready: %w", err)
	}

	// Pick the conversation back up after a suspend and resume.
	if b, err := os.ReadFile(sessionPointer()); err == nil {
		if path := strings.TrimSpace(string(b)); path != "" {
			if _, statErr := os.Stat(path); statErr == nil {
				resp, err := p.request(ready, map[string]any{"type": "switch_session", "sessionPath": path})
				switch {
				case err != nil:
					logger.Warn("could not restore the previous pi session", "path", path, "error", err)
				case resp.Data.Cancelled:
					logger.Warn("restoring the previous pi session was cancelled", "path", path)
				default:
					logger.Info("restored the previous pi session", "path", path)
				}
			}
		}
	}
	return p, nil
}

// rememberSession records Pi's current session file so a later process can resume it.
func (p *piProcess) rememberSession(ctx context.Context, logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := p.request(ctx, map[string]any{"type": "get_state"})
	if err != nil || resp.Data.SessionFile == "" {
		logger.Warn("could not read pi session file", "error", err)
		return
	}
	if err := os.WriteFile(sessionPointer(), []byte(resp.Data.SessionFile), 0o644); err != nil {
		logger.Warn("could not record pi session file", "error", err)
	}
}

// piExecutor maps A2A requests onto Pi's RPC protocol.
//
// kagent wraps this executor: the wrapper emits the task's first event and
// publishes the final one, so the executor emits neither a submitted task nor
// anything after its terminal status.
type piExecutor struct {
	logger *slog.Logger
	mu     sync.Mutex // one turn at a time: a Pi process holds one session
	pi     *piProcess
	last   time.Time
}

// position gives each event an increasing ordering key, as kagent's own
// harnesses do, so the UI shows events in order.
func (e *piExecutor) position() time.Time {
	t := time.Now().UTC()
	if !t.After(e.last) {
		t = e.last.Add(time.Nanosecond)
	}
	e.last = t
	return t
}

func messageText(m *a2atype.Message) string {
	if m == nil {
		return ""
	}
	var sb strings.Builder
	for _, part := range m.Parts {
		sb.WriteString(part.Text())
	}
	return sb.String()
}

func (e *piExecutor) failed(ec *a2asrv.ExecutorContext, reason string) *a2atype.TaskStatusUpdateEvent {
	msg := a2atype.NewMessage(a2atype.MessageRoleAgent, a2atype.NewTextPart(reason))
	msg.TaskID, msg.ContextID = ec.TaskID, ec.ContextID
	apia2a.SetTimelinePosition(msg, e.position())
	return a2atype.NewStatusUpdateEvent(ec, a2atype.TaskStateFailed, msg)
}

func (e *piExecutor) Execute(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2atype.Event, error] {
	return func(yield func(a2atype.Event, error) bool) {
		e.mu.Lock()
		defer e.mu.Unlock()

		if !yield(a2atype.NewStatusUpdateEvent(ec, a2atype.TaskStateWorking, nil), nil) {
			return
		}
		if e.pi == nil {
			p, err := startPi(ctx, e.logger)
			if err != nil {
				e.logger.Error("could not start pi", "error", err)
				yield(e.failed(ec, "could not start pi: "+err.Error()), nil)
				return
			}
			e.pi = p
		}
		pi := e.pi

		// Drop anything left over from an earlier turn before sending the prompt.
	drain:
		for {
			select {
			case <-pi.events:
			default:
				break drain
			}
		}

		promptID := "turn-" + string(ec.TaskID)
		prompt := map[string]any{"id": promptID, "type": "prompt", "message": messageText(ec.Message)}
		if err := pi.send(prompt); err != nil {
			e.pi = nil // restart Pi on the next turn
			yield(e.failed(ec, "could not reach pi: "+err.Error()), nil)
			return
		}

		var artifactID a2atype.ArtifactID
		accepted := false
		for {
			select {
			case <-ctx.Done():
				_ = pi.send(map[string]any{"type": "abort"})
				yield(a2atype.NewStatusUpdateEvent(ec, a2atype.TaskStateCanceled, nil), nil)
				return
			case ev, ok := <-pi.events:
				if !ok {
					e.pi = nil // Pi exited; restart it on the next turn
					yield(e.failed(ec, "pi exited unexpectedly"), nil)
					return
				}
				switch {
				case ev.Type == "response" && ev.ID == promptID:
					if ev.Success != nil && !*ev.Success {
						yield(e.failed(ec, ev.Error), nil)
						return
					}
					accepted = true
				case ev.Type == "message_update" && ev.AssistantMessageEvent.Type == "text_delta":
					if ev.AssistantMessageEvent.Delta == "" {
						continue
					}
					var update *a2atype.TaskArtifactUpdateEvent
					if artifactID == "" {
						update = a2atype.NewArtifactEvent(ec, a2atype.NewTextPart(ev.AssistantMessageEvent.Delta))
						artifactID = update.Artifact.ID
					} else {
						update = a2atype.NewArtifactUpdateEvent(ec, artifactID, a2atype.NewTextPart(ev.AssistantMessageEvent.Delta))
					}
					apia2a.SetTimelinePosition(update.Artifact, e.position())
					if !yield(update, nil) {
						return
					}
				case ev.Type == "agent_settled" && accepted:
					// Save the session pointer before the terminal event: the
					// actor can suspend as soon as the task settles.
					pi.rememberSession(ctx, e.logger)
					yield(a2atype.NewStatusUpdateEvent(ec, a2atype.TaskStateCompleted, nil), nil)
					return
				}
			}
		}
	}
}

func (e *piExecutor) Cancel(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2atype.Event, error] {
	return func(yield func(a2atype.Event, error) bool) {
		if p := e.pi; p != nil {
			_ = p.send(map[string]any{"type": "abort"})
		}
		yield(a2atype.NewStatusUpdateEvent(ec, a2atype.TaskStateCanceled, nil), nil)
	}
}

func main() {
	logger, err := logging.NewFromEnv(os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// kagent renders the agent card from the AgentTemplate and passes it in.
	var card a2atype.AgentCard
	if err := json.Unmarshal([]byte(os.Getenv(agentCardEnv)), &card); err != nil || strings.TrimSpace(card.Name) == "" {
		logger.Error("a valid agent card is required", "env", agentCardEnv, "error", err)
		os.Exit(1)
	}

	application, err := app.New(app.AppConfig{
		AgentCard: card,
		Port:      privatePort,
		AppName:   card.Name,
		Logger:    logger,
	}, &piExecutor{logger: logger})
	if err != nil {
		logger.Error("could not construct the A2A app", "error", err)
		os.Exit(1)
	}
	if err := application.Run(); err != nil {
		logger.Error("adapter stopped", "error", err)
		os.Exit(1)
	}
}
