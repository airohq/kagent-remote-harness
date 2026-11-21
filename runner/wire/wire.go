// Package wire is the protocol between the remote harness, in a kagent
// sandbox, and the runner on a machine outside the cluster. The harness POSTs
// a TurnRequest to TurnPath; the runner answers with newline-delimited Events
// and ends with a "done" or "failure" Event. See docs/protocol.md.
package wire

const (
	// Version is the protocol version. Every answer from a runner carries it
	// in ProtocolHeader, which tells the runner's answers from a transport's,
	// such as a tunnel's "endpoint offline" page.
	Version        = "1"
	ProtocolHeader = "Remote-Harness-Protocol"

	InfoPath    = "/v1/info"
	TurnPath    = "/v1/turn"
	ContentType = "application/x-ndjson"

	// MaxRequestBytes limits a turn request's body.
	MaxRequestBytes = 1 << 20
	// MaxEventBytes limits one encoded event. The runner cuts the text or
	// result of a larger event short.
	MaxEventBytes = 1 << 20
)

// Info is the answer to InfoPath.
type Info struct {
	Protocol  string   `json:"protocol"`
	Machine   string   `json:"machine"`
	Harnesses []string `json:"harnesses"`
}

// TurnRequest is one user message for the runner's coding agent.
type TurnRequest struct {
	Prompt string `json:"prompt"`
	// ContinuationID is the coding agent's session (Claude Code) or thread
	// (Codex) to resume. Empty starts a new one.
	ContinuationID string `json:"continuation_id,omitempty"`
	// Instructions is the Agent's system prompt.
	Instructions string `json:"instructions,omitempty"`
	// Model is the model from the Agent's ModelConfig. Empty uses the coding
	// agent's default.
	Model string `json:"model,omitempty"`
}

// Event types.
const (
	EventSession    = "session"
	EventText       = "text"
	EventToolCall   = "tool_call"
	EventToolResult = "tool_result"
	EventFailure    = "failure"
	EventDone       = "done"
)

// Event is one line of the runner's answer to a turn.
type Event struct {
	Type           string         `json:"type"`
	ContinuationID string         `json:"continuation_id,omitempty"`
	Machine        string         `json:"machine,omitempty"`
	Text           string         `json:"text,omitempty"`
	ID             string         `json:"id,omitempty"`
	Name           string         `json:"name,omitempty"`
	Arguments      map[string]any `json:"arguments,omitempty"`
	Result         any            `json:"result,omitempty"`
	IsError        bool           `json:"is_error,omitempty"`
	Message        string         `json:"message,omitempty"`
}
