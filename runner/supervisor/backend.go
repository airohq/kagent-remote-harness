// Package supervisor provides execution backends for on-demand workers.
package supervisor

import (
	"context"
	"io"

	"github.com/airohq/kagent-remote-harness/runner"
	"github.com/airohq/kagent-remote-harness/runner/wire"
)

// Backend owns the execution environment for a runner. RunTurn streams a
// single turn and honors cancellation. Close releases the backend's resources.
type Backend interface {
	RunTurn(context.Context, wire.TurnRequest, runner.Emit) error
	io.Closer
}

// OnBackend keeps the selected coding agent's metadata and delegates turn
// execution to the backend. The HTTP runner still handles auth and concurrency.
func OnBackend(h runner.Harness, b Backend) runner.Harness {
	return hostedHarness{Harness: h, backend: b}
}

type hostedHarness struct {
	runner.Harness
	backend Backend
}

func (h hostedHarness) Run(ctx context.Context, _ runner.Process, turn wire.TurnRequest, emit runner.Emit) error {
	return h.backend.RunTurn(ctx, turn, emit)
}
