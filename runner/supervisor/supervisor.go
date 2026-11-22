package supervisor

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/airohq/kagent-remote-harness/runner"
)

// Supervisor serves the authenticated runner protocol. It starts its backend
// on demand and keeps the workspace identified by name across restarts.
type Supervisor struct {
	handler http.Handler
	backend Backend
	closing context.Context
	cancel  context.CancelFunc
	mu      sync.RWMutex
}

// New constructs a local supervisor without starting a worker or a listener.
// The caller owns the transport and must call Close when it stops serving.
func New(name, token string, opts Options, logger *slog.Logger) (*Supervisor, error) {
	h, err := opts.Adapter()
	if err != nil {
		return nil, err
	}
	machine, err := os.Hostname()
	if err != nil {
		return nil, err
	}
	b, err := openBackend(name, opts)
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	if opts.Verbose {
		var names []string
		for _, entry := range providerEnv(opts.Harness) {
			key, _, _ := strings.Cut(entry, "=")
			names = append(names, key)
		}
		logger.Info("sandbox configured", "sandbox", opts.Backend, "harness", opts.Harness, "image", opts.Image, "credential_names", names)
	}
	handler, err := runner.New(runner.Config{Harness: OnBackend(h, b), Token: token, Machine: machine + "-" + name, Workdir: ".", Env: []string{}, TurnTimeout: opts.TurnTimeout, Verbose: opts.Verbose, Logger: logger})
	if err != nil {
		return nil, errors.Join(err, b.Close())
	}
	closing, cancel := context.WithCancel(context.Background())
	return &Supervisor{handler: handler, backend: b, closing: closing, cancel: cancel}, nil
}

func (s *Supervisor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closing.Err() != nil {
		http.Error(w, "runner stopped", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(s.closing, cancel)
	defer stop()
	s.handler.ServeHTTP(w, r.WithContext(ctx))
}

// Close cancels active turns and waits for their cleanup before closing the backend.
func (s *Supervisor) Close() error {
	s.cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.backend == nil {
		return nil
	}
	err := s.backend.Close()
	s.backend = nil
	return err
}
