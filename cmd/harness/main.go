// Command harness runs in a kagent sandbox as a BYO harness and sends each
// turn to a remote harness runner, at REMOTE_HARNESS_URL.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/kagent-dev/kagent/go/adk/pkg/app"
	contract "github.com/kagent-dev/kagent/go/api/bootstrap"
	runtimea2a "github.com/kagent-dev/kagent/go/harness/runtime/a2a"
	"github.com/kagent-dev/kagent/go/harness/runtime/continuation"
	"github.com/kagent-dev/kagent/go/pkg/logging"
	"github.com/kagent-dev/kagent/go/pkg/tracing"

	"github.com/airohq/kagent-remote-harness/internal/harness"
)

// kagent connects to a BYO harness on port 80 only.
const privatePort = "80"

func main() {
	logger, err := logging.New(os.Stderr, "info")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := run(logger); err != nil {
		logger.Error("remote harness stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	url := os.Getenv("REMOTE_HARNESS_URL")
	if url == "" {
		return errors.New("set REMOTE_HARNESS_URL to the runner's URL in the Harness's env")
	}
	wait := 2 * time.Minute
	if v := os.Getenv("REMOTE_HARNESS_WAIT"); v != "" {
		var err error
		if wait, err = time.ParseDuration(v); err != nil {
			return fmt.Errorf("REMOTE_HARNESS_WAIT: %w", err)
		}
	}
	cfg, card, err := harness.LoadBundle(context.Background(), os.Getenv, contract.Directory)
	if err != nil {
		return err
	}
	// The coding agent's session lives on the runner's machine; the Actor
	// keeps only its ID and machine in /data, like kagent's own harnesses.
	dataDir := os.Getenv("KAGENT_DATA_DIR")
	if dataDir == "" {
		dataDir = "/data"
	}
	store, err := continuation.New(filepath.Join(dataDir, "remote-harness"), "remote-harness", harness.ValidateContinuation)
	if err != nil {
		return err
	}
	agent := cfg.GetAgent()
	relay := &harness.Relay{
		URL:          url,
		Instructions: agent.GetInstruction(),
		Model:        agent.GetModel().GetModel(),
		Wait:         wait,
		// No timeout: a turn lasts as long as the coding agent works, up to
		// the runner's --turn-timeout.
		HTTP:   &http.Client{},
		Logger: logger,
	}
	executor, err := runtimea2a.New(relay, store, tracing.RuntimeTelemetry{})
	if err != nil {
		return err
	}
	logger.Info("remote harness starting", "runner", url, "model", relay.Model)
	application, err := app.New(app.AppConfig{AgentCard: card, Port: privatePort, AppName: card.Name, ControlPlaneURL: cfg.GetRuntime().GetControlPlaneUrl(), Logger: logger}, executor)
	if err != nil {
		return fmt.Errorf("construct A2A app: %w", err)
	}
	return application.Run()
}
