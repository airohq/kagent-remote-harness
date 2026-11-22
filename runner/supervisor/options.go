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
	HarnessArgs                                 []string
}

// BindFlags registers coding-agent settings shared with the worker command.
func (o *WorkerOptions) BindFlags(fs *flag.FlagSet) {
	fs.StringVar(&o.Harness, "harness", "", "coding agent: claude or codex (required)")
	fs.StringVar(&o.ClaudePermissionMode, "claude-permission-mode", "", "Claude Code permission mode")
	fs.StringVar(&o.CodexSandbox, "codex-sandbox", "", "filesystem access Codex requests inside the sandbox (e.g. read-only or workspace-write)")
	fs.Func("harness-arg", "one native coding-agent argument; repeat for each argument", func(arg string) error {
		o.HarnessArgs = append(o.HarnessArgs, arg)
		return nil
	})
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
	mode, extra, err := nativeArgs(o.Harness, mode, o.HarnessArgs)
	if err != nil {
		return nil, err
	}
	return runner.NewHarness(o.Harness, mode, extra)
}

// Options selects the execution backend and configures the worker.
type Options struct {
	WorkerOptions
	Verbose bool
	Backend string
	Image   string
	// CodexLoginVolume optionally names an existing engine volume labelled with
	// CodexLoginLabel. Its auth directory holds the file-backed Codex login.
	CodexLoginVolume string
	loginVolume      string
}

const defaultWorkerImage = "ghcr.io/airohq/kagent-remote-harness/runner:latest"

// BindFlags installs the runner settings and their defaults.
func (o *Options) BindFlags(fs *flag.FlagSet) {
	o.WorkerOptions.BindFlags(fs)
	fs.BoolVar(&o.Verbose, "verbose", false, "show sandbox configuration, credential names, and turn event diagnostics")
	fs.StringVar(&o.Backend, "sandbox", "container", "execution environment for the coding agent")
	fs.StringVar(&o.Backend, "runner", "container", "legacy spelling of --sandbox")
	fs.StringVar(&o.Image, "image", defaultWorkerImage, "worker image (the default is pulled on the first turn; overrides use a local image)")
}

// openBackend selects how turns run. Backend-specific startup belongs here.
// The listener and the Tunnel use the same protocol for every implementation.
func openBackend(name string, opts Options) (Backend, error) {
	switch opts.Backend {
	case "container":
		args := []string{"worker", "--harness", opts.Harness,
			"--codex-sandbox", opts.CodexSandbox, "--claude-permission-mode", opts.ClaudePermissionMode,
			"--idle-timeout", opts.IdleTimeout.String(), "--turn-timeout", opts.TurnTimeout.String()}
		for _, arg := range opts.HarnessArgs {
			args = append(args, "--harness-arg="+arg)
		}
		c, err := NewContainer(opts.Harness, name, opts.Image, opts.IdleTimeout, args)
		if err != nil {
			return nil, err
		}
		c.loginVolume = opts.loginVolume
		if c.loginVolume == "" && opts.Harness == "codex" {
			c.loginVolume = opts.CodexLoginVolume
			c.optionalLogin = true
		}
		return c, nil
	default:
		return nil, fmt.Errorf("unknown sandbox %q: available sandboxes: container", opts.Backend)
	}
}
