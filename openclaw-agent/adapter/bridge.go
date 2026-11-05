package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	gatewayReadyTimeout = 120 * time.Second
	acpStartTimeout     = 60 * time.Second
	recoveryTimeout     = 120 * time.Second
)

// openclawBridge owns the two OpenClaw processes in the actor: the gateway,
// which holds the agent loop, and the ACP bridge, which the runner drives.
type openclawBridge struct {
	logger    *slog.Logger
	env       []string // environment shared by both processes
	workspace string
	errs      *errorTap

	mu        sync.Mutex
	gateway   *exec.Cmd
	gwExited  chan struct{}
	client    *acpClient
	sessionID string
}

func hasEnv(env []string, name string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, name+"=") {
			return true
		}
	}
	return false
}

func newBridge(logger *slog.Logger, env []string, workspace string) (*openclawBridge, error) {
	token := make([]byte, 24)
	if _, err := rand.Read(token); err != nil {
		return nil, err
	}
	// The token only lets the ACP bridge talk to the gateway on loopback. It
	// lives in the environment of the two processes and is never written to disk.
	env = append(env, "OPENCLAW_GATEWAY_TOKEN="+hex.EncodeToString(token))
	// OpenClaw writes files through a native helper. Under gVisor that helper
	// fails with no error code, and OpenClaw reports "path is not a regular
	// file under root" for every file it tries to create. Its JavaScript
	// fallback works, so use that unless the Harness says otherwise.
	if !hasEnv(env, "FS_SAFE_NATIVE_MODE") {
		env = append(env, "FS_SAFE_NATIVE_MODE=off")
	}
	return &openclawBridge{logger: logger, env: env, workspace: workspace, errs: &errorTap{}}, nil
}

// startGatewayLocked runs `openclaw gateway run` and waits until it reports ready.
func (b *openclawBridge) startGatewayLocked(ctx context.Context) error {
	logResources(b.logger, b.workspace)
	cmd := exec.Command("openclaw", "gateway", "run")
	cmd.Env = b.env
	cmd.Dir = b.workspace
	// Gateway logs go to the actor log, and the tap watches them for run errors.
	out := io.MultiWriter(os.Stderr, b.errs)
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the OpenClaw gateway: %w", err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	b.gateway, b.gwExited = cmd, exited

	ready, cancel := context.WithTimeout(ctx, gatewayReadyTimeout)
	defer cancel()
	for {
		select {
		case <-exited:
			return fmt.Errorf("the OpenClaw gateway exited while starting")
		case <-ready.Done():
			_ = cmd.Process.Kill()
			return fmt.Errorf("the OpenClaw gateway was not ready after %s", gatewayReadyTimeout)
		case <-time.After(500 * time.Millisecond):
		}
		req, _ := http.NewRequestWithContext(ready, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/readyz", gatewayPort), nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				b.logger.Info("OpenClaw gateway is ready")
				return nil
			}
		}
	}
}

// awaitRecovery waits until the gateway has finished re-running any session that
// was interrupted when the last gateway died. A prompt sent meanwhile is steered
// into that run and ends with no output. It gives up after recoveryTimeout, because
// a stuck recovery must not hold the turn forever.
func (b *openclawBridge) awaitRecovery(ctx context.Context) {
	deadline := time.After(recoveryTimeout)
	for b.errs.recoveryPending() {
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			b.logger.Warn("OpenClaw was still recovering an interrupted session; continuing", "after", recoveryTimeout)
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// session returns a live ACP client and session, starting whatever is missing.
func (b *openclawBridge) session(ctx context.Context) (*acpClient, string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// OpenClaw starts on the first turn, not before the actor reports ready.
	// Substrate resumes a suspended actor from its golden snapshot with /data
	// replaced by the saved copy, so a gateway running at golden time would be
	// holding state files that were swapped under it. A gateway started here
	// always sees the /data of the conversation it serves.
	if b.gateway == nil || closed(b.gwExited) {
		if b.gateway != nil {
			b.logger.Warn("the OpenClaw gateway had exited; restarting it")
		}
		b.client = nil
		if err := b.startGatewayLocked(ctx); err != nil {
			return nil, "", err
		}
	}
	if b.client != nil {
		if !closed(b.client.done) {
			return b.client, b.sessionID, nil
		}
		b.client = nil
	}

	// A fixed session key keeps the conversation across restarts: the gateway
	// stores its transcript in /data, and a new bridge for the same key resumes it.
	cmd := exec.Command("openclaw", "acp", "--session", continuationKey)
	cmd.Env = append(append([]string(nil), b.env...), "OPENCLAW_HIDE_BANNER=1", "OPENCLAW_SUPPRESS_NOTES=1")
	cmd.Dir = b.workspace
	cmd.Stderr = os.Stderr
	client, err := startACP(cmd)
	if err != nil {
		return nil, "", fmt.Errorf("start the OpenClaw ACP bridge: %w", err)
	}
	setup, cancel := context.WithTimeout(ctx, acpStartTimeout)
	defer cancel()
	if err := client.call(setup, "initialize", map[string]any{
		"protocolVersion": 1, "clientCapabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "kagent-openclaw-adapter", "version": "1"},
	}, nil); err != nil {
		client.close()
		return nil, "", fmt.Errorf("ACP initialize: %w", err)
	}
	var created struct {
		SessionID string `json:"sessionId"`
	}
	if err := client.call(setup, "session/new", map[string]any{"cwd": b.workspace, "mcpServers": []any{}}, &created); err != nil || created.SessionID == "" {
		client.close()
		return nil, "", fmt.Errorf("ACP session/new: %v", err)
	}
	b.client, b.sessionID = client, created.SessionID
	return client, created.SessionID, nil
}

func closed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func (b *openclawBridge) reset(c *acpClient) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.client == c {
		b.client = nil
	}
	go c.close()
}

func (b *openclawBridge) modelError(since time.Time) string { return b.errs.since(since) }

// errorTap watches the gateway log for failed runs. OpenClaw ends a turn with
// no text when the model call fails, so the log is the only place the reason is.
type errorTap struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	at   time.Time
	text string

	// OpenClaw re-runs a session that was running when its gateway died. The
	// tap follows that recovery from the log: whether it began at all, how many
	// runs it started, and how many have ended. A gateway with nothing to
	// recover logs none of this.
	recoverySeen    bool
	recoveryKnown   bool
	recoveryStarted int
	recoveryEnded   int
}

// runError matches the line OpenClaw logs when a run ends in an error, such as
// "... isError=true model=... error=<message> rawError=<provider body>".
var runError = regexp.MustCompile(`isError=true\b.*? error=(.+?)(?: rawError=|$)`)

// recoveryStart is logged once at start-up, with the number of runs recovery began.
// recoveryEnd is logged when each of those runs ends.
var (
	recoveryAny   = regexp.MustCompile(`\[main-session-restart-recovery\]`)
	recoveryStart = regexp.MustCompile(`restart recovery startup complete: started=(\d+)`)
	recoveryEnd   = regexp.MustCompile(`restart recovery terminal:`)
)

func (e *errorTap) Write(p []byte) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.buf.Write(p)
	for {
		line, err := e.buf.ReadBytes('\n')
		if err != nil {
			e.buf.Write(line) // keep the partial line for the next write
			break
		}
		line = bytes.TrimRight(line, "\r\n")
		if m := runError.FindSubmatch(line); m != nil {
			e.at, e.text = time.Now(), string(m[1])
		}
		if recoveryAny.Match(line) {
			e.recoverySeen = true
		}
		if m := recoveryStart.FindSubmatch(line); m != nil {
			e.recoveryKnown = true
			e.recoveryStarted, _ = strconv.Atoi(string(m[1]))
		}
		if recoveryEnd.Match(line) {
			e.recoveryEnded++
		}
	}
	return len(p), nil
}

// recoveryPending reports whether the gateway began a start-up recovery that has
// not finished. It is false when the gateway logged no recovery at all.
func (e *errorTap) recoveryPending() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.recoverySeen {
		return false
	}
	return !e.recoveryKnown || e.recoveryEnded < e.recoveryStarted
}

func (e *errorTap) since(t time.Time) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.at.Before(t) {
		return ""
	}
	return e.text
}
