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
	"syscall"
	"time"

	"github.com/airohq/kagent-remote-harness/runner"
	"github.com/airohq/kagent-remote-harness/runner/supervisor"
	"github.com/airohq/kagent-remote-harness/wire"
)

func supervise(args []string, logger *slog.Logger) (err error) {
	fs := flag.NewFlagSet("supervise", flag.ContinueOnError)
	var opts supervisor.Options
	opts.BindFlags(fs)
	name := fs.String("name", os.Getenv("REMOTE_HARNESS_NAME"), "persistent runner name (the container backend uses it for the volume)")
	socket := fs.String("socket", os.Getenv("REMOTE_HARNESS_SOCKET"), "Unix socket to serve in a private directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected supervisor argument %q", fs.Arg(0))
	}
	if *socket == "" {
		return errors.New("set --socket to a path in a private directory")
	}
	token, err := readToken("")
	if err != nil {
		return err
	}
	host, err := supervisor.New(*name, token, opts, logger)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, host.Close()) }()
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
	server := &http.Server{Handler: host, ReadHeaderTimeout: 10 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() { <-ctx.Done(); server.Close() }()
	err = server.Serve(listener)
	cancel()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}

// worker is PID 1 in the runner image. The engine's attached stdin/stdout
// carry one turn at a time. EOF or idle expiry also cleans up orphaned workers.
func worker(args []string, logger *slog.Logger) error {
	fs := flag.NewFlagSet("worker", flag.ContinueOnError)
	var opts supervisor.WorkerOptions
	opts.BindFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	h, err := opts.Adapter()
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
		case <-time.After(opts.IdleTimeout + time.Minute):
			return nil
		case turn := <-input:
			turnCtx, stop := context.WithTimeout(ctx, opts.TurnTimeout)
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
