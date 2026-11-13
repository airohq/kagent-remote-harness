// Package runner runs a remote harness's turns with a local coding agent. A
// Runner is an http.Handler: it checks each request's token and runs the
// turn's coding agent CLI, so a program can serve it over any transport.
package runner

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/airohq/kagent-remote-harness/wire"
)

const (
	// MinTokenLength is the shortest token a Runner accepts: 32 characters,
	// such as 16 random bytes in hex.
	MinTokenLength = 32
	// failedAuthPerMinute is how many requests with a wrong token the Runner
	// answers with 401 a minute. It answers more with 429 until the minute
	// passes. Behind a tunnel every request comes from the tunnel, so the
	// limit is for the whole Runner.
	failedAuthPerMinute = 10
)

// Config configures a Runner.
type Config struct {
	// Harness runs the coding agent.
	Harness Harness
	// Token is the bearer token every request must carry.
	Token string
	// Machine names this machine. It ties a conversation to the machine that
	// keeps its working copy and session.
	Machine string
	// Workdir is the directory the coding agent works in.
	Workdir string
	// TurnTimeout limits one turn. Zero means no limit.
	TurnTimeout time.Duration
	// LogContent logs prompts and the coding agent's output. Without it, the
	// log has only turn IDs, timing and results.
	LogContent bool
	// Env is the coding agent's environment. Nil uses this process's
	// environment without the runner's own REMOTE_HARNESS_* variables, so the
	// coding agent can't read the token.
	Env []string
	// Logger defaults to slog.Default.
	Logger *slog.Logger
}

// Runner serves the protocol in package wire.
type Runner struct {
	cfg  Config
	mux  *http.ServeMux
	busy atomic.Bool

	authMu     sync.Mutex
	authWindow time.Time
	authFailed int
	now        func() time.Time
	token      []byte
}

// New checks cfg and returns a Runner.
func New(cfg Config) (*Runner, error) {
	switch {
	case cfg.Harness == nil:
		return nil, errors.New("runner: a harness is required")
	case len(cfg.Token) < MinTokenLength:
		return nil, fmt.Errorf("runner: the token must have at least %d characters", MinTokenLength)
	case cfg.Machine == "":
		return nil, errors.New("runner: a machine name is required")
	case cfg.Workdir == "":
		return nil, errors.New("runner: a working directory is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Env == nil {
		cfg.Env = CodingAgentEnv(os.Environ())
	}
	r := &Runner{cfg: cfg, now: time.Now, token: []byte(cfg.Token)}
	r.mux = http.NewServeMux()
	r.mux.HandleFunc("GET "+wire.InfoPath, r.info)
	r.mux.HandleFunc("POST "+wire.TurnPath, r.turn)
	return r, nil
}

// CodingAgentEnv returns env without the runner's REMOTE_HARNESS_* variables.
func CodingAgentEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if !strings.HasPrefix(kv, "REMOTE_HARNESS_") {
			out = append(out, kv)
		}
	}
	return out
}

func (r *Runner) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	w.Header().Set(wire.ProtocolHeader, wire.Version)
	if status := r.authorize(req); status != http.StatusOK {
		http.Error(w, http.StatusText(status), status)
		return
	}
	r.mux.ServeHTTP(w, req)
}

// authorize compares the bearer token in constant time and limits requests
// with a wrong one.
func (r *Runner) authorize(req *http.Request) int {
	got, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
	if ok && subtle.ConstantTimeCompare([]byte(got), r.token) == 1 {
		return http.StatusOK
	}
	r.authMu.Lock()
	defer r.authMu.Unlock()
	if now := r.now(); now.Sub(r.authWindow) >= time.Minute {
		r.authWindow, r.authFailed = now, 0
	}
	r.authFailed++
	if r.authFailed > failedAuthPerMinute {
		return http.StatusTooManyRequests
	}
	r.cfg.Logger.Warn("request with a wrong token", "remote", req.RemoteAddr, "path", req.URL.Path)
	return http.StatusUnauthorized
}

func (r *Runner) info(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(wire.Info{Protocol: wire.Version, Machine: r.cfg.Machine, Harnesses: []string{r.cfg.Harness.Name()}})
}

func (r *Runner) turn(w http.ResponseWriter, req *http.Request) {
	var turn wire.TurnRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, wire.MaxRequestBytes)).Decode(&turn); err != nil {
		if tooLarge := new(http.MaxBytesError); errors.As(err, &tooLarge) {
			http.Error(w, "the turn request is over 1 MiB", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "decode the turn request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(turn.Prompt) == "" {
		http.Error(w, "the turn has no prompt", http.StatusBadRequest)
		return
	}
	if !r.busy.CompareAndSwap(false, true) {
		http.Error(w, "the runner is busy with another turn", http.StatusConflict)
		return
	}
	defer r.busy.Store(false)

	id := turnID()
	logger := r.cfg.Logger.With("turn", id)
	attrs := []any{"harness", r.cfg.Harness.Name(), "resume", turn.ContinuationID != "", "model", turn.Model}
	if r.cfg.LogContent {
		attrs = append(attrs, "prompt", turn.Prompt)
	}
	logger.Info("turn started", attrs...)
	started := time.Now()

	w.Header().Set("Content-Type", wire.ContentType)
	w.WriteHeader(http.StatusOK)
	emit := newEmitter(w, r.cfg.Machine, logger, r.cfg.LogContent)

	ctx := req.Context()
	if r.cfg.TurnTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.cfg.TurnTimeout)
		defer cancel()
	}
	proc := Process{Dir: r.cfg.Workdir, Env: r.cfg.Env, Logger: logger, LogContent: r.cfg.LogContent}
	err := r.cfg.Harness.Run(ctx, proc, turn, emit)
	switch {
	case err == nil:
		emit(wire.Event{Type: wire.EventDone})
		logger.Info("turn done", "duration", time.Since(started).Round(time.Millisecond))
	case errors.Is(ctx.Err(), context.DeadlineExceeded) && req.Context().Err() == nil:
		msg := fmt.Sprintf("The turn ran longer than %s, the runner's --turn-timeout, so the runner stopped the coding agent.", r.cfg.TurnTimeout)
		emit(wire.Event{Type: wire.EventFailure, Message: msg})
		logger.Warn("turn timed out", "duration", time.Since(started).Round(time.Millisecond))
	case req.Context().Err() != nil:
		logger.Info("turn cancelled", "duration", time.Since(started).Round(time.Millisecond))
	default:
		emit(wire.Event{Type: wire.EventFailure, Message: err.Error()})
		// The error can quote the coding agent's output.
		attrs := []any{"duration", time.Since(started).Round(time.Millisecond)}
		if r.cfg.LogContent {
			attrs = append(attrs, "error", err)
		}
		logger.Warn("turn failed", attrs...)
	}
}

func turnID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
