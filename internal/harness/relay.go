package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/kagent-dev/kagent/go/harness/runtime"

	"github.com/airohq/kagent-remote-harness/wire"
)

// placeholderToken is what the harness sends in Authorization. kagent's
// egress gateway replaces it with the token from the EgressPolicy's Secret,
// so the real token never enters the sandbox.
const placeholderToken = "placeholder"

// maxLineBytes limits one line of the runner's answer.
const maxLineBytes = 16 << 20

// Relay sends each turn to the runner at URL.
type Relay struct {
	// URL is the runner's base URL, such as https://johndoe.ngrok-free.app.
	URL string
	// Instructions is the Agent's system prompt.
	Instructions string
	// Model is the model from the Agent's ModelConfig; empty uses the coding
	// agent's default.
	Model string
	// Wait is how long a turn waits for an offline or busy runner.
	Wait   time.Duration
	HTTP   *http.Client
	Logger *slog.Logger
}

// Run implements kagent's runtime.Runner.
func (r *Relay) Run(ctx context.Context, turn runtime.Turn, sink runtime.EventSink) (runtime.Outcome, error) {
	if turn.InputResponse != nil {
		return fail("The remote harness doesn't ask for input.")
	}
	session, machine := SplitContinuation(turn.ContinuationID)
	body, err := json.Marshal(wire.TurnRequest{Prompt: turn.Prompt, ContinuationID: session, Instructions: r.Instructions, Model: r.Model})
	if err != nil {
		return fail("Encode the turn: %v", err)
	}
	resp, outcome, err := r.send(ctx, body)
	if resp == nil {
		return outcome, err
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), maxLineBytes)
	for scanner.Scan() {
		var ev wire.Event
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			return fail("Decode the runner's event: %v", err)
		}
		var err error
		switch ev.Type {
		case wire.EventSession:
			if ev.ContinuationID == "" || ev.Machine == "" {
				return fail("The runner started a session without its ID or machine.")
			}
			if machine != "" && ev.Machine != machine {
				return fail("This conversation started on machine %q, but the runner at %s is machine %q. The conversation's working copy and session are on %q: start the runner there again, or start a new conversation.", machine, r.URL, ev.Machine, machine)
			}
			err = sink.SessionStarted(runtime.SessionStarted{ContinuationID: JoinContinuation(ev.ContinuationID, ev.Machine)})
		case wire.EventText:
			err = sink.TextDelta(runtime.TextDelta{Text: ev.Text})
		case wire.EventToolCall:
			err = sink.ToolCall(runtime.ToolCall{ID: ev.ID, Name: ev.Name, Arguments: ev.Arguments})
		case wire.EventToolResult:
			err = sink.ToolResult(runtime.ToolResult{ID: ev.ID, Name: ev.Name, Result: ev.Result, IsError: ev.IsError})
		case wire.EventFailure:
			return fail("%s", ev.Message)
		case wire.EventDone:
			return runtime.Outcome{}, nil
		}
		if err != nil {
			return runtime.Outcome{}, err
		}
	}
	if ctx.Err() != nil {
		return runtime.Outcome{}, ctx.Err()
	}
	if err := scanner.Err(); err != nil {
		return fail("The connection to the runner broke: %v", err)
	}
	return fail("The runner's answer ended without a result.")
}

// send POSTs the turn. It retries while the runner is unreachable or busy:
// a failed connection, an answer without the protocol header (such as a
// tunnel's "endpoint offline" page), or 409. It returns the runner's 200
// answer, or the turn's outcome.
func (r *Relay) send(ctx context.Context, body []byte) (*http.Response, runtime.Outcome, error) {
	deadline := time.Now().Add(r.Wait)
	var last string
	for backoff := time.Second; ; backoff = min(2*backoff, 15*time.Second) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(r.URL, "/")+wire.TurnPath, bytes.NewReader(body))
		if err != nil {
			o, err := fail("Build the request to the runner: %v", err)
			return nil, o, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", wire.ContentType)
		req.Header.Set("Authorization", "Bearer "+placeholderToken)
		resp, err := r.HTTP.Do(req)
		switch {
		case ctx.Err() != nil:
			if resp != nil {
				resp.Body.Close()
			}
			return nil, runtime.Outcome{}, ctx.Err()
		case err != nil:
			last = err.Error()
		case resp.Header.Get(wire.ProtocolHeader) == "":
			last = fmt.Sprintf("%s answered %s, not the runner", r.URL, resp.Status)
			resp.Body.Close()
		case resp.StatusCode == http.StatusConflict:
			last = "the runner is busy with another turn"
			resp.Body.Close()
		case resp.StatusCode == http.StatusOK:
			return resp, runtime.Outcome{}, nil
		default:
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
			resp.Body.Close()
			o, err := fail("The runner answered %s: %s", resp.Status, strings.TrimSpace(string(msg)))
			return nil, o, err
		}
		if time.Now().Add(backoff).After(deadline) {
			o, err := fail("The runner at %s is offline or busy: it hasn't taken the turn for %s (%s). Start the runner, or wait for its turn to end, and send the message again.", r.URL, r.Wait, last)
			return nil, o, err
		}
		r.Logger.Info("waiting for the runner", "reason", last, "retry", backoff)
		select {
		case <-ctx.Done():
			return nil, runtime.Outcome{}, ctx.Err()
		case <-time.After(backoff):
		}
	}
}

func fail(format string, args ...any) (runtime.Outcome, error) {
	return runtime.Outcome{Failure: &runtime.Failure{Message: fmt.Sprintf(format, args...)}}, nil
}

// JoinContinuation stores the machine with the coding agent's session in
// kagent's continuation ID, so a later turn knows which machine keeps the
// conversation's work.
func JoinContinuation(session, machine string) string { return session + "@" + machine }

// SplitContinuation is the inverse of JoinContinuation.
func SplitContinuation(id string) (session, machine string) {
	if i := strings.LastIndexByte(id, '@'); i >= 0 {
		return id[:i], id[i+1:]
	}
	return id, ""
}

// ValidateContinuation accepts the continuation IDs JoinContinuation makes.
func ValidateContinuation(id string) error {
	session, machine := SplitContinuation(id)
	if session == "" || machine == "" || len(id) > 512 || strings.ContainsAny(id, "\n\r\t ") {
		return errors.New("invalid continuation ID")
	}
	return nil
}
