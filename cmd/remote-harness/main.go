// Command remote-harness runs a kagent agent's turns on this machine, with a
// local coding agent such as Claude Code or Codex.
//
//	remote-harness serve --harness claude --claude-permission-mode acceptEdits
//
// It listens on a port, checks each request's token, and runs the coding
// agent's CLI for each turn. Expose the port to the cluster with a tunnel,
// such as ngrok. See https://github.com/airohq/kagent-remote-harness.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/airohq/kagent-remote-harness/runner"
)

// version is set at build time.
var version = "dev"

const usage = `remote-harness runs a kagent agent's turns on this machine.

Usage:
  remote-harness serve --harness <claude|codex> [flags]
  remote-harness version

Run "remote-harness serve -h" for its flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
		if err := serve(os.Args[2:], logger); err != nil {
			logger.Error("remote-harness stopped", "error", err)
			os.Exit(1)
		}
	case "version":
		fmt.Println(version)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, " ") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func serve(args []string, logger *slog.Logger) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := fs.String("listen", envOr("REMOTE_HARNESS_LISTEN", ":8080"), "address to listen on")
	workdir := fs.String("workdir", envOr("REMOTE_HARNESS_WORKDIR", "."), "directory the coding agent works in")
	stateDir := fs.String("state-dir", os.Getenv("REMOTE_HARNESS_STATE_DIR"), "directory that keeps the machine's name across restarts; empty keeps nothing")
	harnessName := fs.String("harness", "", "coding agent: claude or codex (required)")
	machine := fs.String("machine", "", "this machine's name (default: the name kept in --state-dir, or the host name)")
	claudeMode := fs.String("claude-permission-mode", "", "Claude Code's --permission-mode, such as acceptEdits (required with claude)")
	codexSandbox := fs.String("codex-sandbox", "", "Codex's --sandbox, such as workspace-write (required with codex)")
	var harnessArgs stringList
	fs.Var(&harnessArgs, "harness-arg", "a flag to add to the coding agent's command; repeatable")
	tokenFile := fs.String("token-file", "", "file holding the token every request must carry (default: $REMOTE_HARNESS_TOKEN)")
	turnTimeout := fs.Duration("turn-timeout", 30*time.Minute, "how long one turn may run")
	logContent := fs.Bool("log-content", false, "log prompts and the coding agent's output")
	if err := fs.Parse(args); err != nil {
		return err
	}

	mode := *claudeMode
	if *harnessName == "codex" {
		mode = *codexSandbox
	}
	h, err := runner.NewHarness(*harnessName, mode, harnessArgs)
	if err != nil {
		return err
	}
	if _, err := exec.LookPath(h.Command()); err != nil {
		return fmt.Errorf("the %s harness needs the %q CLI on PATH: %w", h.Name(), h.Command(), err)
	}
	token, err := readToken(*tokenFile)
	if err != nil {
		return err
	}
	if err := prepareDirs(*workdir, *stateDir); err != nil {
		return err
	}
	name, err := machineName(*machine, *stateDir)
	if err != nil {
		return err
	}
	workdirAbs, err := filepath.Abs(*workdir)
	if err != nil {
		return err
	}

	r, err := runner.New(runner.Config{
		Harness: h, Token: token, Machine: name, Workdir: workdirAbs,
		TurnTimeout: *turnTimeout, LogContent: *logContent, Logger: logger,
	})
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: *listen, Handler: r, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	logger.Info("serving", "version", version, "listen", *listen, "harness", h.Name(), "machine", name, "workdir", workdirAbs)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// readToken reads the token from file, or from $REMOTE_HARNESS_TOKEN.
func readToken(file string) (string, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("read the token: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	if t := strings.TrimSpace(os.Getenv("REMOTE_HARNESS_TOKEN")); t != "" {
		return t, nil
	}
	return "", errors.New("set the token with --token-file or REMOTE_HARNESS_TOKEN, such as `openssl rand -hex 32`")
}

// prepareDirs creates the working directory, the state directory, and the
// directories the coding agents keep their sessions in, such as on a fresh
// volume or a tmpfs home directory.
func prepareDirs(workdir, stateDir string) error {
	dirs := []string{workdir, stateDir, os.Getenv("HOME"), os.Getenv("CLAUDE_CONFIG_DIR"), os.Getenv("CODEX_HOME")}
	for _, d := range dirs {
		if d == "" {
			continue
		}
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", d, err)
		}
	}
	return nil
}

// machineName is flag, else the name kept in stateDir, else the host name,
// which it keeps in stateDir. A container gets a new host name on every
// start, so the kept name ties conversations to the volume, not the
// container.
func machineName(flagValue, stateDir string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	path := ""
	if stateDir != "" {
		path = filepath.Join(stateDir, "machine")
		if b, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(b)) != "" {
			return strings.TrimSpace(string(b)), nil
		}
	}
	name, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("find the host name; set --machine: %w", err)
	}
	if path != "" {
		if err := os.WriteFile(path, []byte(name+"\n"), 0o600); err != nil {
			return "", fmt.Errorf("keep the machine name: %w", err)
		}
	}
	return name, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
