package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/airohq/kagent-remote-harness/runner/wire"
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

// An explicit local model overrides the turn's model without emitting the
// option twice. Codex also accepts the short -m form.
func hasModelArg(args []string, short bool) bool {
	for _, arg := range args {
		if arg == "--model" || strings.HasPrefix(arg, "--model=") || (short && strings.HasPrefix(arg, "-m")) {
			return true
		}
	}
	return false
}

// Claude runs `claude -p` with stream-json output and partial messages, so
// text streams as the model writes it.
type Claude struct {
	PermissionMode string
	ExtraArgs      []string
}

func (Claude) Name() string    { return "claude" }
func (Claude) Command() string { return "claude" }

// ClaudeEntrypoint is the CLAUDE_CODE_ENTRYPOINT the runner gives Claude Code.
const ClaudeEntrypoint = "kagent-remote-harness"

// claudeCleanSettings turns off hooks: a turn runs no command it was not
// asked to, and starts faster.
const claudeCleanSettings = `{"disableAllHooks":true}`

// claudeManagedMCPConfigs are where an organization's managed MCP servers
// live. Claude Code rejects --strict-mcp-config when one exists.
var claudeManagedMCPConfigs = []string{
	"/Library/Application Support/ClaudeCode/managed-mcp.json",
	"/etc/claude-code/managed-mcp.json",
	`C:\Program Files\ClaudeCode\managed-mcp.json`,
}

func (c Claude) Args(turn wire.TurnRequest, _ string) ([]string, string) {
	args := []string{"-p", "--output-format", "stream-json", "--verbose", "--include-partial-messages", "--permission-mode", c.PermissionMode}
	// A turn starts clean: without the person's own MCP servers, settings,
	// plugins and hooks. The project's settings stay; they belong to the work.
	if !hasArg(c.ExtraArgs, "--strict-mcp-config") && !managedMCPConfig() {
		args = append(args, "--strict-mcp-config")
	}
	if !hasArg(c.ExtraArgs, "--setting-sources") {
		args = append(args, "--setting-sources", "project")
	}
	if !hasArg(c.ExtraArgs, "--settings") {
		args = append(args, "--settings", claudeCleanSettings)
	}
	if turn.Model != "" && !hasModelArg(c.ExtraArgs, false) {
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

func hasArg(args []string, name string) bool {
	for _, arg := range args {
		if arg == name || strings.HasPrefix(arg, name+"=") {
			return true
		}
	}
	return false
}

func managedMCPConfig() bool {
	for _, path := range claudeManagedMCPConfigs {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

// ClaudeEnv returns env for a Claude Code turn: Claude Code reports the
// runner as its entrypoint, and connects to neither claude.ai's MCP servers
// nor an IDE.
func ClaudeEnv(env []string) []string {
	out := make([]string, 0, len(env)+3)
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "CLAUDE_CODE_ENTRYPOINT", "ENABLE_CLAUDEAI_MCP_SERVERS", "CLAUDE_CODE_AUTO_CONNECT_IDE":
			continue
		}
		out = append(out, kv)
	}
	return append(out,
		"CLAUDE_CODE_ENTRYPOINT="+ClaudeEntrypoint,
		"ENABLE_CLAUDEAI_MCP_SERVERS=false",
		"CLAUDE_CODE_AUTO_CONNECT_IDE=0",
	)
}

func (c Claude) Run(ctx context.Context, p Process, turn wire.TurnRequest, emit Emit) error {
	args, stdin := c.Args(turn, p.Dir)
	p.Env = ClaudeEnv(p.Env)
	var result *claudeMessage
	parser := newClaudeParser(emit)
	err := p.Stream(ctx, c.Command(), args, stdin, func(line []byte) {
		var m claudeMessage
		if json.Unmarshal(line, &m) != nil {
			return // not a message, such as a warning
		}
		if m.Type == "result" {
			result = &m
			return
		}
		parser.parse(m)
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
	Type            string `json:"type"`
	Subtype         string `json:"subtype"`
	SessionID       string `json:"session_id"`
	ParentToolUseID string `json:"parent_tool_use_id"`
	IsError         bool   `json:"is_error"`
	Result          string `json:"result"`
	Event           struct {
		Type    string `json:"type"`
		Index   int    `json:"index"`
		Message struct {
			ID string `json:"id"`
		} `json:"message"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	} `json:"event"`
	Message struct {
		ID      string `json:"id"`
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

// claudeParser turns Claude Code's messages into events. With partial
// messages, Claude Code streams each text block's deltas, then repeats the
// whole block in an assistant message. The parser emits the deltas, and from
// the assistant message only text it did not stream. As kagent's own Claude
// harness does, it keys a text block by its message ID and index.
type claudeParser struct {
	emit Emit
	// threads holds the stream of the main conversation ("") and of each
	// subagent, by parent_tool_use_id.
	threads map[string]*claudeThread
	// toolNames names tool calls by ID: Claude's tool results carry only the
	// tool's ID, and kagent wants its name.
	toolNames map[string]string
}

type claudeThread struct {
	messageID string
	// active is the content block being streamed, or -1.
	active int
	// text is what was emitted of each text block, by "message:index".
	text map[string]string
}

func newClaudeParser(emit Emit) *claudeParser {
	return &claudeParser{emit: emit, threads: map[string]*claudeThread{}, toolNames: map[string]string{}}
}

func (p *claudeParser) thread(parent string) *claudeThread {
	t := p.threads[parent]
	if t == nil {
		t = &claudeThread{active: -1, text: map[string]string{}}
		p.threads[parent] = t
	}
	return t
}

func (p *claudeParser) parse(m claudeMessage) {
	switch m.Type {
	case "system":
		if m.Subtype == "init" && m.SessionID != "" {
			p.emit(wire.Event{Type: wire.EventSession, ContinuationID: m.SessionID})
		}
	case "stream_event":
		t := p.thread(m.ParentToolUseID)
		ev := m.Event
		switch ev.Type {
		case "message_start":
			t.messageID, t.active = ev.Message.ID, -1
		case "content_block_start":
			t.active = ev.Index
		case "content_block_delta":
			if ev.Delta.Type == "text_delta" && ev.Delta.Text != "" {
				t.text[blockKey(t.messageID, ev.Index)] += ev.Delta.Text
				p.emit(wire.Event{Type: wire.EventText, Text: ev.Delta.Text})
			}
		case "content_block_stop":
			if t.active == ev.Index {
				t.active = -1
			}
		case "message_stop":
			t.active = -1
		}
	case "assistant":
		t := p.thread(m.ParentToolUseID)
		if id := m.Message.ID; id != "" && id != t.messageID {
			t.messageID, t.active = id, -1
		}
		for i, c := range m.Message.Content {
			switch c.Type {
			case "text":
				index := i
				// Claude Code sends an assistant message for the block it is
				// streaming, with that block alone.
				if len(m.Message.Content) == 1 && t.active >= 0 {
					index = t.active
				}
				p.text(t, blockKey(t.messageID, index), c.Text)
			case "tool_use":
				if _, seen := p.toolNames[c.ID]; seen {
					continue
				}
				p.toolNames[c.ID] = c.Name
				p.emit(wire.Event{Type: wire.EventToolCall, ID: c.ID, Name: c.Name, Arguments: c.Input})
			}
		}
	case "user":
		for _, c := range m.Message.Content {
			if c.Type == "tool_result" {
				p.emit(wire.Event{Type: wire.EventToolResult, ID: c.ToolUseID, Name: p.toolNames[c.ToolUseID], Result: c.Content, IsError: c.IsError})
			}
		}
	}
}

// text emits what a block's final text adds to what was streamed of it.
// Events only append, so a final text that rewrites the streamed text adds
// nothing.
func (p *claudeParser) text(t *claudeThread, key, final string) {
	streamed := t.text[key]
	if len(final) <= len(streamed) || !strings.HasPrefix(final, streamed) {
		return
	}
	t.text[key] = final
	p.emit(wire.Event{Type: wire.EventText, Text: final[len(streamed):]})
}

func blockKey(messageID string, index int) string {
	return messageID + ":" + strconv.Itoa(index)
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
	if turn.Model != "" && !hasModelArg(c.ExtraArgs, true) {
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
			switch ev.Item.Type {
			case "command_execution":
				emit(wire.Event{Type: wire.EventToolCall, ID: ev.Item.ID, Name: "shell", Arguments: map[string]any{"command": ev.Item.Command}})
			case "mcp_tool_call":
				emit(wire.Event{Type: wire.EventToolCall, ID: ev.Item.ID, Name: codexMCPName(ev.Item.Server, ev.Item.Tool), Arguments: ev.Item.Arguments})
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
			case "web_search":
				// The query is known only when the search has finished.
				results := make([]map[string]any, 0, len(ev.Item.Results))
				for _, r := range ev.Item.Results {
					results = append(results, map[string]any{"title": r.Title, "url": r.URL})
				}
				emit(wire.Event{Type: wire.EventToolCall, ID: ev.Item.ID, Name: "web_search", Arguments: map[string]any{"query": ev.Item.Query}})
				emit(wire.Event{Type: wire.EventToolResult, ID: ev.Item.ID, Name: "web_search", Result: results})
			case "mcp_tool_call":
				result, failed := codexMCPResult(ev)
				emit(wire.Event{Type: wire.EventToolResult, ID: ev.Item.ID, Name: codexMCPName(ev.Item.Server, ev.Item.Tool), Result: result, IsError: failed})
			}
		case "turn.completed":
			completed = true
		case "turn.failed":
			failure = ev.Error.Message
		case "error":
			failure = ev.Message
		}
	})
	// A terminal provider error is more useful than the CLI's exit status
	// and stderr tail. Codex can also emit retry errors and later succeed.
	if ctx.Err() == nil && failure != "" && !completed {
		return fmt.Errorf("codex: %s", failure)
	}
	if err != nil {
		return err
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
		// web_search
		Query   string `json:"query"`
		Results []struct {
			Title string `json:"title"`
			URL   string `json:"url"`
		} `json:"results"`
		// mcp_tool_call
		Server    string         `json:"server"`
		Tool      string         `json:"tool"`
		Arguments map[string]any `json:"arguments"`
		Result    *struct {
			Content           []codexContent `json:"content"`
			StructuredContent any            `json:"structured_content"`
		} `json:"result"`
		CallError *struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"item"`
}

type codexContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// codexMCPName names an MCP tool as Claude Code does, mcp__server__tool, the
// form kagent reads as an MCP tool.
func codexMCPName(server, tool string) string {
	return "mcp__" + server + "__" + tool
}

// codexMCPResult is an MCP call's text, or its structured content when it
// has no text, and whether the call failed.
func codexMCPResult(ev codexEvent) (any, bool) {
	failed := ev.Item.Status == "failed" || ev.Item.CallError != nil
	if ev.Item.CallError != nil && ev.Item.CallError.Message != "" {
		return ev.Item.CallError.Message, true
	}
	if ev.Item.Result == nil {
		return nil, failed
	}
	var texts []string
	for _, c := range ev.Item.Result.Content {
		if c.Type == "text" {
			texts = append(texts, c.Text)
		}
	}
	if len(texts) == 0 && ev.Item.Result.StructuredContent != nil {
		return ev.Item.Result.StructuredContent, failed
	}
	return strings.Join(texts, "\n"), failed
}
