// Command openclaw-a2a-adapter runs OpenClaw as a kagent harness.
//
// kagent talks A2A to a bring-your-own harness, and OpenClaw talks ACP, so this
// adapter sits between them. It writes OpenClaw's configuration from the
// AgentTemplate kagent compiled, starts the OpenClaw gateway, and drives
// `openclaw acp` for each turn. kagent's shared harness executor does the A2A
// side: streaming, task state, and pausing a task while OpenClaw waits for an
// exec approval.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/kagent-dev/kagent/go/adk/pkg/app"
	"github.com/kagent-dev/kagent/go/api/adk"
	runtimea2a "github.com/kagent-dev/kagent/go/harness/runtime/a2a"
	"github.com/kagent-dev/kagent/go/harness/runtime/continuation"
	"github.com/kagent-dev/kagent/go/pkg/logging"
	"github.com/kagent-dev/kagent/go/pkg/tracing"
)

const (
	configEnv    = "KAGENT_CONFIG_JSON"     // rendered by kagent from the AgentTemplate
	agentCardEnv = "KAGENT_AGENT_CARD_JSON" // rendered by kagent from the AgentTemplate
	dataDir      = "/data"                  // durable directory in the actor
	privatePort  = "80"                     // fixed by the kagent A2A contract
)

var continuationPattern = regexp.MustCompile(`^agent:[a-z0-9_-]+:[a-z0-9_-]+$`)

func main() {
	logger, err := logging.NewFromEnv(os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := run(context.Background(), logger); err != nil {
		logger.Error("openclaw adapter stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, logger *slog.Logger) error {
	var card a2atype.AgentCard
	if err := json.Unmarshal([]byte(os.Getenv(agentCardEnv)), &card); err != nil || strings.TrimSpace(card.Name) == "" {
		return fmt.Errorf("a valid agent card is required in %s: %v", agentCardEnv, err)
	}
	var cfg adk.AgentConfig
	if err := json.Unmarshal([]byte(os.Getenv(configEnv)), &cfg); err != nil {
		return fmt.Errorf("a valid agent configuration is required in %s: %w", configEnv, err)
	}

	workspace := filepath.Join(dataDir, "workspace")
	stateDir := filepath.Join(dataDir, "openclaw") // OpenClaw's sessions and memory index
	homeDir := filepath.Join(dataDir, "home")
	// The config is rebuilt on every start from the AgentTemplate, so it lives
	// outside the durable volume.
	configDir := "/tmp/kagent-openclaw"
	for _, dir := range []string{workspace, stateDir, homeDir, configDir, filepath.Join(dataDir, "adapter")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}

	doc, err := buildConfig(&cfg, workspace, os.Getenv(execModeEnv), os.Getenv)
	if err != nil {
		return err
	}
	raw, err := marshalConfig(doc)
	if err != nil {
		return err
	}
	configPath := filepath.Join(configDir, "openclaw.json")
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		return err
	}
	if text := instructions(&cfg); text != "" {
		if err := os.WriteFile(filepath.Join(workspace, "AGENTS.md"), []byte(text), 0o644); err != nil {
			return err
		}
	}

	bridge, err := newBridge(logger, append(os.Environ(),
		"HOME="+homeDir,
		"OPENCLAW_STATE_DIR="+stateDir,
		"OPENCLAW_CONFIG_PATH="+configPath,
		"OPENCLAW_DISABLE_BONJOUR=1",
		"OPENCLAW_NO_AUTO_UPDATE=1",
		"DO_NOT_TRACK=1",
	), workspace)
	if err != nil {
		return err
	}
	store, err := continuation.New(filepath.Join(dataDir, "adapter"), "openclaw", func(id string) error {
		if !continuationPattern.MatchString(id) {
			return fmt.Errorf("invalid OpenClaw session key %q", id)
		}
		return nil
	})
	if err != nil {
		return err
	}
	executor, err := runtimea2a.New(&runner{logger: bridge.logger, bridge: bridge}, store, tracing.RuntimeTelemetry{})
	if err != nil {
		return err
	}
	application, err := app.New(app.AppConfig{
		AgentCard: card,
		Port:      privatePort,
		AppName:   card.Name,
		Logger:    bridge.logger,
	}, executor)
	if err != nil {
		return fmt.Errorf("construct the A2A app: %w", err)
	}
	return application.Run()
}

// logResources records what the actor has to work with. OpenClaw's gateway uses
// close to 1 GiB, and Substrate sizes an actor, so this is the first thing to
// read when a turn is slow or fails.
func logResources(logger *slog.Logger, dir string) {
	mem, _ := os.ReadFile("/proc/meminfo")
	var total, free string
	for _, line := range strings.Split(string(mem), "\n") {
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			total = strings.Join(strings.Fields(line)[1:], " ")
		case strings.HasPrefix(line, "MemAvailable:"):
			free = strings.Join(strings.Fields(line)[1:], " ")
		}
	}
	logger.Info("actor resources", "memTotal", total, "memAvailable", free, "cpus", runtime.NumCPU(), "dataDir", dir)
}
