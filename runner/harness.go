package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/airohq/kagent-remote-harness/wire"
)

// Harness runs one turn with a coding agent CLI.
type Harness interface {
	// Name is the --harness value, such as "claude".
	Name() string
	// Command is the CLI's name, such as "claude".
	Command() string
	// Args returns the CLI's arguments and stdin for a turn.
	Args(turn wire.TurnRequest, workdir string) (args []string, stdin string)
	// Run runs the turn and emits its events, except the final "done" or
	// "failure", which the Runner sends.
	Run(ctx context.Context, p Process, turn wire.TurnRequest, emit Emit) error
}

// NewHarness returns the harness for name. mode is Claude Code's permission
// mode or Codex's sandbox; extra are flags added to the CLI's command.
func NewHarness(name, mode string, extra []string) (Harness, error) {
	var flag string
	switch name {
	case "claude":
		flag = "--claude-permission-mode"
	case "codex":
		flag = "--codex-sandbox"
	case "":
		return nil, errors.New("choose a harness with --harness: claude or codex")
	default:
		return nil, fmt.Errorf("unknown harness %q: want claude or codex", name)
	}
	if mode == "" {
		return nil, fmt.Errorf("set %s: nothing can ask you for approvals, so choose what the coding agent does without asking", flag)
	}
	if name == "claude" {
		return Claude{PermissionMode: mode, ExtraArgs: extra}, nil
	}
	return Codex{Sandbox: mode, ExtraArgs: extra}, nil
}

// Claude runs `claude -p` with stream-json output.
type Claude struct {
	PermissionMode string
	ExtraArgs      []string
}

func (Claude) Name() string    { return "claude" }
func (Claude) Command() string { return "claude" }

func (c Claude) Args(turn wire.TurnRequest, _ string) ([]string, string) {
	args := []string{"-p", "--output-format", "stream-json", "--verbose", "--permission-mode", c.PermissionMode}
	if turn.Model != "" {
		args = append(args, "--model", turn.Model)
	}
	if turn.Instructions != "" {
		args = append(args, "--append-system-prompt", turn.Instructions)
	}
	args = append(args, c.ExtraArgs...)
	if turn.ContinuationID != "" {
		args = append(args, "--resume", turn.ContinuationID)
	}
	return args, turn.Prompt
}

func (c Claude) Run(ctx context.Context, p Process, turn wire.TurnRequest, emit Emit) error {
	args, stdin := c.Args(turn, p.Dir)
	var result *claudeMessage
	// Claude's tool results carry only the tool's ID; kagent wants its name.
	toolNames := map[string]string{}
	err := p.Stream(ctx, c.Command(), args, stdin, func(line []byte) {
		var m claudeMessage
		if json.Unmarshal(line, &m) != nil {
			return // not a message, such as a warning
		}
		switch m.Type {
		case "system":
			if m.Subtype == "init" && m.SessionID != "" {
				emit(wire.Event{Type: wire.EventSession, ContinuationID: m.SessionID})
			}
		case "assistant":
			for _, c := range m.Message.Content {
				switch c.Type {
				case "text":
					emit(wire.Event{Type: wire.EventText, Text: c.Text})
				case "tool_use":
					toolNames[c.ID] = c.Name
					emit(wire.Event{Type: wire.EventToolCall, ID: c.ID, Name: c.Name, Arguments: c.Input})
				}
			}
		case "user":
			for _, c := range m.Message.Content {
				if c.Type == "tool_result" {
					emit(wire.Event{Type: wire.EventToolResult, ID: c.ToolUseID, Name: toolNames[c.ToolUseID], Result: c.Content, IsError: c.IsError})
				}
			}
		case "result":
			result = &m
		}
	})
	if err != nil {
		return err
	}
	if result == nil {
		return errors.New("claude ended without a result")
	}
	if result.IsError {
		return fmt.Errorf("claude: %s %s", result.Subtype, result.Result)
	}
	return nil
}

type claudeMessage struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	IsError   bool   `json:"is_error"`
	Result    string `json:"result"`
	Message   struct {
		Content []struct {
			Type      string         `json:"type"`
			Text      string         `json:"text"`
			ID        string         `json:"id"`
			Name      string         `json:"name"`
			Input     map[string]any `json:"input"`
			ToolUseID string         `json:"tool_use_id"`
			Content   any            `json:"content"`
			IsError   bool           `json:"is_error"`
		} `json:"content"`
	} `json:"message"`
}

// Codex runs `codex exec --json`, resuming the thread when there is one.
type Codex struct {
	Sandbox   string
	ExtraArgs []string
}

func (Codex) Name() string    { return "codex" }
func (Codex) Command() string { return "codex" }

func (c Codex) Args(turn wire.TurnRequest, workdir string) ([]string, string) {
	args := []string{"exec", "--json", "--skip-git-repo-check", "-C", workdir}
	if turn.Model != "" {
		args = append(args, "-m", turn.Model)
	}
	prompt := turn.Prompt
	if turn.ContinuationID != "" {
		// resume takes no --sandbox; pass it as config.
		args = append(args, "-c", fmt.Sprintf("sandbox_mode=%q", c.Sandbox))
		args = append(args, c.ExtraArgs...)
		args = append(args, "resume", turn.ContinuationID, "-")
		return args, prompt
	}
	// codex exec has no system prompt flag; start the thread with it.
	if turn.Instructions != "" {
		prompt = turn.Instructions + "\n\n" + prompt
	}
	args = append(args, "--sandbox", c.Sandbox)
	args = append(args, c.ExtraArgs...)
	return append(args, "-"), prompt
}

func (c Codex) Run(ctx context.Context, p Process, turn wire.TurnRequest, emit Emit) error {
	args, stdin := c.Args(turn, p.Dir)
	var failure string
	completed := false
	err := p.Stream(ctx, c.Command(), args, stdin, func(line []byte) {
		var ev codexEvent
		if json.Unmarshal(line, &ev) != nil {
			return
		}
		switch ev.Type {
		case "thread.started":
			emit(wire.Event{Type: wire.EventSession, ContinuationID: ev.ThreadID})
		case "item.started":
			if ev.Item.Type == "command_execution" {
				emit(wire.Event{Type: wire.EventToolCall, ID: ev.Item.ID, Name: "shell", Arguments: map[string]any{"command": ev.Item.Command}})
			}
		case "item.completed":
			switch ev.Item.Type {
			case "agent_message":
				emit(wire.Event{Type: wire.EventText, Text: ev.Item.Text})
			case "command_execution":
				emit(wire.Event{Type: wire.EventToolResult, ID: ev.Item.ID, Name: "shell", Result: ev.Item.AggregatedOutput,
					IsError: ev.Item.ExitCode != nil && *ev.Item.ExitCode != 0})
			case "file_change":
				emit(wire.Event{Type: wire.EventToolCall, ID: ev.Item.ID, Name: "file_change", Arguments: map[string]any{"changes": ev.Item.Changes}})
				emit(wire.Event{Type: wire.EventToolResult, ID: ev.Item.ID, Name: "file_change", Result: ev.Item.Status})
			}
		case "turn.completed":
			completed = true
		case "turn.failed":
			failure = ev.Error.Message
		case "error":
			failure = ev.Message
		}
	})
	if err != nil {
		return err
	}
	if failure != "" {
		return fmt.Errorf("codex: %s", failure)
	}
	if !completed {
		return errors.New("codex ended without completing the turn")
	}
	return nil
}

type codexEvent struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Message  string `json:"message"`
	Error    struct {
		Message string `json:"message"`
	} `json:"error"`
	Item struct {
		ID               string `json:"id"`
		Type             string `json:"type"`
		Text             string `json:"text"`
		Command          string `json:"command"`
		AggregatedOutput string `json:"aggregated_output"`
		ExitCode         *int   `json:"exit_code"`
		Status           string `json:"status"`
		Changes          any    `json:"changes"`
	} `json:"item"`
}
