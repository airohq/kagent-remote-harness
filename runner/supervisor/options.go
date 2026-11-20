package supervisor

import (
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/airohq/kagent-remote-harness/runner"
)

// WorkerOptions are shared by the host and the worker inside the image.
type WorkerOptions struct {
	Harness, ClaudePermissionMode, CodexSandbox string
	IdleTimeout, TurnTimeout                    time.Duration
}

// BindFlags registers coding-agent settings shared with the worker command.
func (o *WorkerOptions) BindFlags(fs *flag.FlagSet) {
	fs.StringVar(&o.Harness, "harness", "", "coding agent: claude or codex (required)")
	fs.StringVar(&o.ClaudePermissionMode, "claude-permission-mode", "", "Claude Code permission mode")
	fs.StringVar(&o.CodexSandbox, "codex-sandbox", "", "filesystem access Codex requests inside the sandbox (e.g. read-only or workspace-write)")
	fs.DurationVar(&o.IdleTimeout, "idle-timeout", 5*time.Minute, "remove the container after this much idle time")
	fs.DurationVar(&o.TurnTimeout, "turn-timeout", 30*time.Minute, "maximum time for one turn")
}

// Adapter validates the settings and selects the coding agent.
func (o WorkerOptions) Adapter() (runner.Harness, error) {
	if o.IdleTimeout <= 0 || o.TurnTimeout <= 0 {
		return nil, errors.New("idle-timeout and turn-timeout must be positive")
	}
	mode := o.ClaudePermissionMode
	if o.Harness == "codex" {
		mode = o.CodexSandbox
	}
	return runner.NewHarness(o.Harness, mode, nil)
}

// Options selects the execution backend and configures the worker.
type Options struct {
	WorkerOptions
	Verbose bool
	Backend string
	Image   string
}

// BindFlags installs the runner settings and their defaults.
func (o *Options) BindFlags(fs *flag.FlagSet) {
	o.WorkerOptions.BindFlags(fs)
	fs.BoolVar(&o.Verbose, "verbose", false, "show sandbox configuration, credential names, and turn event diagnostics")
	fs.StringVar(&o.Backend, "sandbox", "container", "execution environment for the coding agent")
	fs.StringVar(&o.Backend, "runner", "container", "legacy spelling of --sandbox")
	fs.StringVar(&o.Image, "image", "ghcr.io/airohq/kagent-remote-harness/runner:latest", "locally available runner image with worker support")
}

// openBackend selects how turns run. Backend-specific startup belongs here.
// The listener and the Tunnel use the same protocol for every implementation.
func openBackend(kind, name, image string, opts WorkerOptions) (Backend, error) {
	switch kind {
	case "container":
		args := []string{"worker", "--harness", opts.Harness,
			"--codex-sandbox", opts.CodexSandbox, "--claude-permission-mode", opts.ClaudePermissionMode,
			"--idle-timeout", opts.IdleTimeout.String(), "--turn-timeout", opts.TurnTimeout.String()}
		return NewContainer(opts.Harness, name, image, opts.IdleTimeout, args)
	default:
		return nil, fmt.Errorf("unknown sandbox %q: available sandboxes: container", kind)
	}
}
