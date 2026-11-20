package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"syscall"
	"time"

	"github.com/moby/moby/client"

	"github.com/airohq/kagent-remote-harness/internal/supervisor"
	"github.com/airohq/kagent-remote-harness/runner"
	"github.com/airohq/kagent-remote-harness/wire"
)

type workerOptions struct {
	harness, claudeMode, codexMode string
	idle, timeout                  time.Duration
}

func (o *workerOptions) flags(fs *flag.FlagSet) {
	fs.StringVar(&o.harness, "harness", "", "coding agent: claude or codex (required)")
	fs.StringVar(&o.claudeMode, "claude-permission-mode", "", "Claude Code permission mode")
	fs.StringVar(&o.codexMode, "codex-sandbox", "", "Codex sandbox mode")
	fs.DurationVar(&o.idle, "idle-timeout", 5*time.Minute, "remove the container after this much idle time")
	fs.DurationVar(&o.timeout, "turn-timeout", 30*time.Minute, "maximum time for one turn")
}

func (o workerOptions) adapter() (runner.Harness, error) {
	if o.idle <= 0 || o.timeout <= 0 {
		return nil, errors.New("idle-timeout and turn-timeout must be positive")
	}
	mode := o.claudeMode
	if o.harness == "codex" {
		mode = o.codexMode
	}
	return runner.NewHarness(o.harness, mode, nil)
}

func supervise(args []string, logger *slog.Logger) error {
	fs := flag.NewFlagSet("supervise", flag.ContinueOnError)
	var opts workerOptions
	opts.flags(fs)
	name := fs.String("name", os.Getenv("REMOTE_HARNESS_NAME"), "name of the persistent runner volume")
	socket := fs.String("socket", os.Getenv("REMOTE_HARNESS_SOCKET"), "Unix socket to serve in a private directory")
	image := fs.String("image", "ghcr.io/airohq/kagent-remote-harness/runner:latest", "locally available runner image with worker support")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected supervisor argument %q", fs.Arg(0))
	}
	if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,120}$`).MatchString(*name) {
		return errors.New("set --name to a volume name of up to 121 letters, digits, dots, underscores or hyphens")
	}
	if *socket == "" {
		return errors.New("set --socket to a path in a private directory")
	}
	h, err := opts.adapter()
	if err != nil {
		return err
	}
	token, err := readToken("")
	if err != nil {
		return err
	}
	engine, err := client.New(client.FromEnv)
	if err != nil {
		return err
	}
	defer engine.Close()
	host, err := os.Hostname()
	if err != nil {
		return err
	}
	backend := &supervisor.Container{Harness: h, Engine: engine, Image: *image, Volume: *name, Idle: opts.idle,
		WorkerArgs: []string{"worker", "--harness", opts.harness, "--codex-sandbox", opts.codexMode, "--claude-permission-mode", opts.claudeMode, "--idle-timeout", opts.idle.String(), "--turn-timeout", opts.timeout.String()}}
	handler, err := runner.New(runner.Config{Harness: backend, Token: token, Machine: host + "-" + *name, Workdir: "/work/workspace", Env: []string{}, TurnTimeout: opts.timeout, Logger: logger})
	if err != nil {
		return err
	}
	listener, err := net.Listen("unix", *socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	if err := os.Chmod(*socket, 0600); err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	// When started by airo, the pipe also closes if the parent is killed.
	if os.Getenv("REMOTE_HARNESS_PARENT_PIPE") == "1" {
		go func() { _, _ = io.Copy(io.Discard, os.Stdin); cancel() }()
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() { <-ctx.Done(); server.Close() }()
	err = server.Serve(listener)
	cancel()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return errors.Join(err, backend.Close())
}

// worker is PID 1 in the runner image. The engine's attached stdin/stdout
// carry one turn at a time. EOF or idle expiry also cleans up orphaned workers.
func worker(args []string, logger *slog.Logger) error {
	fs := flag.NewFlagSet("worker", flag.ContinueOnError)
	var opts workerOptions
	opts.flags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	h, err := opts.adapter()
	if err != nil {
		return err
	}
	dir := envOr("REMOTE_HARNESS_WORKDIR", "/work/workspace")
	if err := prepareDirs(dir, os.Getenv("REMOTE_HARNESS_STATE_DIR")); err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	input := make(chan wire.TurnRequest)
	go func() {
		defer cancel()
		s := bufio.NewScanner(os.Stdin)
		s.Buffer(make([]byte, 64<<10), 8*wire.MaxRequestBytes)
		for s.Scan() {
			var turn wire.TurnRequest
			if err := json.Unmarshal(s.Bytes(), &turn); err != nil {
				return
			}
			select {
			case input <- turn:
			case <-ctx.Done():
				return
			}
		}
	}()
	proc := runner.Process{Dir: dir, Env: runner.CodingAgentEnv(os.Environ()), Logger: logger}
	emit := func(ev wire.Event) {
		b, err := runner.EncodeEvent(ev)
		if err == nil {
			_, err = os.Stdout.Write(append(b, '\n'))
		}
		if err != nil {
			cancel()
		}
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(opts.idle + time.Minute):
			return nil
		case turn := <-input:
			turnCtx, stop := context.WithTimeout(ctx, opts.timeout)
			err := h.Run(turnCtx, proc, turn, emit)
			stop()
			if err != nil {
				emit(wire.Event{Type: wire.EventFailure, Message: err.Error()})
			} else {
				emit(wire.Event{Type: wire.EventDone})
			}
		}
	}
}
