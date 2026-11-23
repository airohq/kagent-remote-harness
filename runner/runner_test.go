package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/airohq/kagent-remote-harness/runner/wire"
)

const testToken = "0123456789abcdef0123456789abcdef"

// fakeClaude writes a `claude` script to a directory on PATH. The script
// records its arguments and environment in dir, then runs body.
func fakeClaude(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + dir + "/args\"\nenv > \"" + dir + "/env\"\ncat > \"" + dir + "/stdin\"\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

const claudeTurn = `echo '{"type":"system","subtype":"init","session_id":"s1"}'
echo 'not json'
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"hello"},{"type":"tool_use","id":"t1","name":"Edit","input":{"file_path":"a.go"}}]}}'
echo '{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}'
echo '{"type":"result","subtype":"success","is_error":false,"result":"hello"}'`

func newTestRunner(t *testing.T, cfg Config) *httptest.Server {
	t.Helper()
	if cfg.Harness == nil {
		cfg.Harness = Claude{PermissionMode: "acceptEdits"}
	}
	if cfg.Token == "" {
		cfg.Token = testToken
	}
	if cfg.Machine == "" {
		cfg.Machine = "johndoe-mbp"
	}
	if cfg.Workdir == "" {
		cfg.Workdir = t.TempDir()
	}
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, ctx context.Context, url, token string, turn wire.TurnRequest) *http.Response {
	t.Helper()
	body, _ := json.Marshal(turn)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url+wire.TurnPath, strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func events(t *testing.T, resp *http.Response) []wire.Event {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %s", resp.Status)
	}
	var out []wire.Event
	s := bufio.NewScanner(resp.Body)
	for s.Scan() {
		var ev wire.Event
		if err := json.Unmarshal(s.Bytes(), &ev); err != nil {
			t.Fatalf("event %q: %v", s.Text(), err)
		}
		out = append(out, ev)
	}
	return out
}

func TestTurnRunsClaudeAndStreamsEvents(t *testing.T) {
	noManagedMCP(t)
	dir := fakeClaude(t, claudeTurn)
	t.Setenv("REMOTE_HARNESS_TOKEN", testToken)
	srv := newTestRunner(t, Config{Harness: Claude{PermissionMode: "acceptEdits", ExtraArgs: []string{"--disallowedTools=Bash"}}})

	evs := events(t, post(t, t.Context(), srv.URL, testToken, wire.TurnRequest{
		Prompt: "fix it", ContinuationID: "s1", Instructions: "be brief", Model: "opus",
	}))

	want := []string{wire.EventSession, wire.EventText, wire.EventToolCall, wire.EventToolResult, wire.EventDone}
	if len(evs) != len(want) {
		t.Fatalf("events = %+v", evs)
	}
	for i, ev := range evs {
		if ev.Type != want[i] {
			t.Errorf("event %d = %s, want %s", i, ev.Type, want[i])
		}
	}
	if evs[0].ContinuationID != "s1" || evs[0].Machine != "johndoe-mbp" {
		t.Errorf("session = %+v", evs[0])
	}
	if evs[3].Name != "Edit" {
		t.Errorf("tool result name = %q", evs[3].Name)
	}

	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	wantArgs := "-p\n--output-format\nstream-json\n--verbose\n--include-partial-messages\n--permission-mode\nacceptEdits\n" +
		"--strict-mcp-config\n--setting-sources\nproject\n--settings\n{\"disableAllHooks\":true}\n" +
		"--model\nopus\n--append-system-prompt\nbe brief\n--disallowedTools=Bash\n--resume\ns1\n"
	if string(args) != wantArgs {
		t.Errorf("args =\n%s\nwant\n%s", args, wantArgs)
	}
	if stdin, _ := os.ReadFile(filepath.Join(dir, "stdin")); string(stdin) != "fix it" {
		t.Errorf("stdin = %q", stdin)
	}
	env, _ := os.ReadFile(filepath.Join(dir, "env"))
	if strings.Contains(string(env), testToken) {
		t.Error("the coding agent's environment holds the runner's token")
	}
	for _, kv := range []string{"CLAUDE_CODE_ENTRYPOINT=" + ClaudeEntrypoint, "ENABLE_CLAUDEAI_MCP_SERVERS=false", "CLAUDE_CODE_AUTO_CONNECT_IDE=0"} {
		if !slices.Contains(strings.Split(string(env), "\n"), kv) {
			t.Errorf("environment lacks %s", kv)
		}
	}
}

func TestClaudeArgsKeepNativeChoices(t *testing.T) {
	noManagedMCP(t)
	args, _ := Claude{PermissionMode: "plan", ExtraArgs: []string{"--setting-sources=user,project", "--settings", "my.json"}}.Args(wire.TurnRequest{Prompt: "x"}, ".")
	want := []string{"-p", "--output-format", "stream-json", "--verbose", "--include-partial-messages", "--permission-mode", "plan",
		"--strict-mcp-config", "--setting-sources=user,project", "--settings", "my.json"}
	if !slices.Equal(args, want) {
		t.Errorf("args = %q, want %q", args, want)
	}

	// Claude Code rejects --strict-mcp-config next to managed MCP servers.
	claudeManagedMCPConfigs = []string{filepath.Join(t.TempDir(), "managed-mcp.json")}
	if err := os.WriteFile(claudeManagedMCPConfigs[0], []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if args, _ := (Claude{PermissionMode: "plan"}).Args(wire.TurnRequest{}, "."); slices.Contains(args, "--strict-mcp-config") {
		t.Errorf("args = %q, want no --strict-mcp-config with managed MCP servers", args)
	}
}

// noManagedMCP makes the test independent of this machine's managed MCP
// servers.
func noManagedMCP(t *testing.T) {
	saved := claudeManagedMCPConfigs
	claudeManagedMCPConfigs = nil
	t.Cleanup(func() { claudeManagedMCPConfigs = saved })
}

func TestClaudeStreamsTextOnce(t *testing.T) {
	lines := []string{
		`{"type":"system","subtype":"init","session_id":"s1"}`,
		// A streamed text block, then its whole text.
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"m1"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"text"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hel"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}}`,
		`{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"hello"}]}}`,
		`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`,
		// A tool call, streamed as JSON, in the same message.
		`{"type":"stream_event","event":{"type":"content_block_start","index":1,"content_block":{"type":"tool_use"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{}"}}}`,
		`{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","id":"t1","name":"Agent","input":{}}]}}`,
		`{"type":"stream_event","event":{"type":"content_block_stop","index":1}}`,
		`{"type":"stream_event","event":{"type":"message_stop"}}`,
		// A subagent's message, not streamed, between the main thread's.
		`{"type":"assistant","parent_tool_use_id":"t1","message":{"id":"m2","content":[{"type":"text","text":"sub"}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`,
		// A block cut short in the stream, completed by its whole text.
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"m3"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"text"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"do"}}}`,
		`{"type":"assistant","message":{"id":"m3","content":[{"type":"text","text":"done"}]}}`,
		// The same message again changes nothing.
		`{"type":"assistant","message":{"id":"m3","content":[{"type":"text","text":"done"}]}}`,
	}
	var got []string
	p := newClaudeParser(func(ev wire.Event) {
		switch ev.Type {
		case wire.EventText:
			got = append(got, ev.Text)
		default:
			got = append(got, ev.Type+":"+ev.Name)
		}
	})
	for _, line := range lines {
		var m claudeMessage
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatal(err)
		}
		p.parse(m)
	}
	want := []string{"session:", "hel", "lo", "tool_call:Agent", "sub", "tool_result:Agent", "do", "ne"}
	if !slices.Equal(got, want) {
		t.Errorf("events = %q, want %q", got, want)
	}
}

func TestAuthorization(t *testing.T) {
	srv := newTestRunner(t, Config{})
	resp := post(t, t.Context(), srv.URL, "wrong-token-wrong-token-wrong-token", wire.TurnRequest{Prompt: "x"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %s, want 401", resp.Status)
	}
	if resp.Header.Get(wire.ProtocolHeader) != wire.Version {
		t.Error("a 401 has no protocol header")
	}
	var last int
	for range failedAuthPerMinute {
		resp := post(t, t.Context(), srv.URL, "", wire.TurnRequest{Prompt: "x"})
		resp.Body.Close()
		last = resp.StatusCode
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("status after %d wrong tokens = %d, want 429", failedAuthPerMinute+1, last)
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+wire.InfoPath, nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var info wire.Info
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if info.Machine != "johndoe-mbp" || info.Protocol != "1" || len(info.Harnesses) != 1 || info.Harnesses[0] != "claude" {
		t.Errorf("info = %+v; the right token must pass after wrong ones", info)
	}
}

func TestRequestLimits(t *testing.T) {
	srv := newTestRunner(t, Config{})
	for name, tc := range map[string]struct {
		turn wire.TurnRequest
		want int
	}{
		"too large": {wire.TurnRequest{Prompt: strings.Repeat("x", wire.MaxRequestBytes)}, http.StatusRequestEntityTooLarge},
		"no prompt": {wire.TurnRequest{}, http.StatusBadRequest},
	} {
		resp := post(t, t.Context(), srv.URL, testToken, tc.turn)
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%s: status = %d, want %d", name, resp.StatusCode, tc.want)
		}
	}
}

func TestBusyRunnerAnswers409(t *testing.T) {
	fakeClaude(t, `echo '{"type":"system","subtype":"init","session_id":"s1"}'; sleep 5`)
	srv := newTestRunner(t, Config{})
	first := post(t, t.Context(), srv.URL, testToken, wire.TurnRequest{Prompt: "slow"})
	defer first.Body.Close()
	// The first turn holds the runner once its session event arrives.
	if _, err := bufio.NewReader(first.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	second := post(t, t.Context(), srv.URL, testToken, wire.TurnRequest{Prompt: "fast"})
	second.Body.Close()
	if second.StatusCode != http.StatusConflict {
		t.Fatalf("status = %s, want 409", second.Status)
	}
}

// The coding agent starts a background process; the turn times out. The
// runner must fail the turn and kill the whole process group.
func TestTurnTimeoutKillsProcessGroup(t *testing.T) {
	dir := fakeClaude(t, `sleep 300 & echo $! > "$(dirname "$0")/child"; sleep 300`)
	srv := newTestRunner(t, Config{TurnTimeout: 500 * time.Millisecond})

	evs := events(t, post(t, t.Context(), srv.URL, testToken, wire.TurnRequest{Prompt: "loop"}))
	if len(evs) != 1 || evs[0].Type != wire.EventFailure || !strings.Contains(evs[0].Message, "--turn-timeout") {
		t.Fatalf("events = %+v", evs)
	}
	assertDead(t, filepath.Join(dir, "child"))
}

func TestCancelledTurnKillsProcessGroup(t *testing.T) {
	dir := fakeClaude(t, `sleep 300 & echo $! > "$(dirname "$0")/child"; echo '{"type":"system","subtype":"init","session_id":"s1"}'; sleep 300`)
	srv := newTestRunner(t, Config{})
	ctx, cancel := context.WithCancel(t.Context())
	resp := post(t, ctx, srv.URL, testToken, wire.TurnRequest{Prompt: "loop"})
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	cancel()
	resp.Body.Close()
	assertDead(t, filepath.Join(dir, "child"))
}

func assertDead(t *testing.T, pidFile string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, err := os.ReadFile(pidFile)
		if err == nil {
			pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
			if pid > 0 && syscall.Kill(pid, 0) != nil {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the coding agent's child process is still running (%s)", b)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestFailedClaudeQuotesStderr(t *testing.T) {
	fakeClaude(t, `echo "not logged in" >&2; exit 1`)
	srv := newTestRunner(t, Config{})
	evs := events(t, post(t, t.Context(), srv.URL, testToken, wire.TurnRequest{Prompt: "x"}))
	if len(evs) != 1 || evs[0].Type != wire.EventFailure || !strings.Contains(evs[0].Message, "not logged in") {
		t.Fatalf("events = %+v", evs)
	}
}

// Claude Code 2.1.323's output when its login's refresh fails.
const claudeSignedOut = `echo '{"type":"system","subtype":"init","session_id":"s1"}'
echo '{"type":"assistant","error":"authentication_failed","is_api_error_message":true,"message":{"id":"m1","model":"<synthetic>","content":[{"type":"text","text":"Failed to authenticate: OAuth session expired and could not be refreshed"}]}}'
echo '{"type":"result","subtype":"success","is_error":true,"result":"Failed to authenticate: OAuth session expired and could not be refreshed"}'
exit 1`

func TestSignedOutClaudeSaysSo(t *testing.T) {
	fakeClaude(t, claudeSignedOut)
	srv := newTestRunner(t, Config{})
	evs := events(t, post(t, t.Context(), srv.URL, testToken, wire.TurnRequest{Prompt: "x"}))
	last := evs[len(evs)-1]
	want := ErrClaudeSignedOut.Error() + ": Failed to authenticate: OAuth session expired and could not be refreshed"
	if last.Type != wire.EventFailure || last.Message != want {
		t.Fatalf("failure = %+v, want %q", last, want)
	}
}

func TestCodexArgs(t *testing.T) {
	c := Codex{Sandbox: "workspace-write", ExtraArgs: []string{"--x"}}
	args, stdin := c.Args(wire.TurnRequest{Prompt: "p", Instructions: "i", Model: "m"}, "/w")
	if got := strings.Join(args, " "); got != "exec --json --skip-git-repo-check -C /w -m m --sandbox workspace-write --x -" || stdin != "i\n\np" {
		t.Errorf("new thread: %q, %q", got, stdin)
	}
	args, stdin = c.Args(wire.TurnRequest{Prompt: "p", Instructions: "i", ContinuationID: "th"}, "/w")
	if got := strings.Join(args, " "); got != `exec --json --skip-git-repo-check -C /w -c sandbox_mode="workspace-write" --x resume th -` || stdin != "p" {
		t.Errorf("resume: %q, %q", got, stdin)
	}
}

func TestNewHarnessNeedsAChoice(t *testing.T) {
	for _, tc := range []struct{ name, mode, want string }{
		{"", "acceptEdits", "--harness"},
		{"claude", "", "--claude-permission-mode"},
		{"codex", "", "--codex-sandbox"},
		{"gemini", "x", "unknown harness"},
	} {
		if _, err := NewHarness(tc.name, tc.mode, nil); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("NewHarness(%q, %q) = %v, want an error about %s", tc.name, tc.mode, err, tc.want)
		}
	}
}

func TestNewRejectsShortToken(t *testing.T) {
	if _, err := New(Config{Harness: Claude{PermissionMode: "x"}, Token: "short", Machine: "m", Workdir: "."}); err == nil {
		t.Fatal("New accepted a short token")
	}
}

func TestCodingAgentEnvDropsRunnerVariables(t *testing.T) {
	got := CodingAgentEnv([]string{"PATH=/bin", "REMOTE_HARNESS_TOKEN=x", "REMOTE_HARNESS_WORKDIR=/w", "HOME=/h"})
	if strings.Join(got, ",") != "PATH=/bin,HOME=/h" {
		t.Errorf("env = %v", got)
	}
}

func TestCutShortKeepsEventsUnderTheLimit(t *testing.T) {
	ev := cutShort(wire.Event{Type: wire.EventToolResult, Result: strings.Repeat("é", wire.MaxEventBytes), Arguments: map[string]any{"a": strings.Repeat("x", wire.MaxEventBytes)}})
	b, _ := json.Marshal(ev)
	if len(b) > wire.MaxEventBytes {
		t.Fatalf("event is %d bytes", len(b))
	}
	if s, _ := ev.Result.(string); !strings.HasSuffix(s, cutNotice) {
		t.Error("the result doesn't say it was cut short")
	}
}

func TestCodexVerboseFailure(t *testing.T) {
	dir := t.TempDir()
	body := `#!/bin/sh
cat >/dev/null
echo '{"type":"turn.failed","error":{"message":"unexpected status 401 Unauthorized: Missing bearer token private-provider-detail"}}'
echo "opaque transport error" >&2
exit 1
`
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte(body), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var logs strings.Builder
	srv := newTestRunner(t, Config{Harness: Codex{Sandbox: "read-only"}, Verbose: true, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	got := events(t, post(t, t.Context(), srv.URL, testToken, wire.TurnRequest{Prompt: "private-prompt"}))
	if len(got) != 1 || got[0].Type != wire.EventFailure || !strings.Contains(got[0].Message, "401") {
		t.Fatalf("expected the provider failure, got %v", got)
	}
	srv.Close()
	log := logs.String()
	if !strings.Contains(log, "provider received no bearer token") || !strings.Contains(log, "type=failure") || strings.Contains(log, "private-") {
		t.Fatalf("unexpected verbose log: %s", log)
	}
}

func TestCodexKeepsMessagesApart(t *testing.T) {
	fakeCodex(t,
		`{"type":"thread.started","thread_id":"th1"}`,
		`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"I'll check."}}`,
		// Neither is an event, so the next message follows this one directly.
		`{"type":"item.completed","item":{"id":"item_1","type":"reasoning","text":""}}`,
		`{"type":"item.completed","item":{"id":"item_2","type":"todo_list","items":[]}}`,
		`{"type":"item.completed","item":{"id":"item_3","type":"agent_message","text":"It is 1.27."}}`,
		// After a tool, kagent starts a new artifact by itself.
		`{"type":"item.started","item":{"id":"item_4","type":"command_execution","command":"go version","status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"item_4","type":"command_execution","command":"go version","aggregated_output":"go1.27","exit_code":0,"status":"completed"}}`,
		`{"type":"item.completed","item":{"id":"item_5","type":"agent_message","text":"Confirmed."}}`,
		`{"type":"turn.completed","usage":{}}`,
	)
	got := codexEvents(t)
	want := []string{
		`{"type":"text","text":"I'll check."}`,
		`{"type":"text","text":"\n\nIt is 1.27."}`,
		`{"type":"tool_call","id":"item_4","name":"shell","arguments":{"command":"go version"}}`,
		`{"type":"tool_result","id":"item_4","name":"shell","result":"go1.27"}`,
		`{"type":"text","text":"Confirmed."}`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("events =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestClaudeKeepsTextBlocksApart(t *testing.T) {
	lines := []string{
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"m1"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"text"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"I'll check."}}}`,
		`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`,
		// Thinking between two text blocks of one message.
		`{"type":"stream_event","event":{"type":"content_block_start","index":1,"content_block":{"type":"thinking"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"thinking_delta","thinking":""}}}`,
		`{"type":"stream_event","event":{"type":"content_block_stop","index":1}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":2,"content_block":{"type":"text"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"It is"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":" 1.27."}}}`,
		`{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"It is 1.27."}]}}`,
		`{"type":"stream_event","event":{"type":"content_block_stop","index":2}}`,
		`{"type":"stream_event","event":{"type":"message_stop"}}`,
	}
	var got []string
	p := newClaudeParser(func(ev wire.Event) { got = append(got, ev.Text) })
	for _, line := range lines {
		var m claudeMessage
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatal(err)
		}
		p.parse(m)
	}
	if want := []string{"I'll check.", "\n\nIt is", " 1.27."}; !slices.Equal(got, want) {
		t.Errorf("text = %q, want %q", got, want)
	}
}

// fakeCodex writes a `codex` script to a directory on PATH that prints lines.
func fakeCodex(t *testing.T, lines ...string) {
	t.Helper()
	dir := t.TempDir()
	body := "#!/bin/sh\ncat >/dev/null\ncat <<'EOF'\n" + strings.Join(lines, "\n") + "\nEOF\n"
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// codexEvents runs a Codex turn and returns its events between the session
// and done, as JSON.
func codexEvents(t *testing.T) []string {
	t.Helper()
	srv := newTestRunner(t, Config{Harness: Codex{Sandbox: "read-only"}})
	evs := events(t, post(t, t.Context(), srv.URL, testToken, wire.TurnRequest{Prompt: "x"}))
	if len(evs) < 2 || evs[0].Type != wire.EventSession || evs[len(evs)-1].Type != wire.EventDone {
		t.Fatalf("events = %+v", evs)
	}
	var got []string
	for _, ev := range evs[1 : len(evs)-1] {
		b, _ := json.Marshal(ev)
		got = append(got, string(b))
	}
	return got
}

func TestCodexReportsSearchesAndMCPCalls(t *testing.T) {
	// Lines as codex exec --json 0.171 prints them.
	fakeCodex(t,
		`{"type":"thread.started","thread_id":"th1"}`,
		`{"type":"item.started","item":{"id":"item_1","type":"web_search","id":"ws1","query":"","action":{"type":"other"}}}`,
		`{"type":"item.completed","item":{"id":"item_1","type":"web_search","id":"ws1","query":"latest Go release","action":{"type":"search","query":"latest Go release"},"results":[{"type":"text_result","title":"Release History","url":"https://go.dev/doc/devel/release","snippet":"…"}]}}`,
		`{"type":"item.started","item":{"id":"item_2","type":"mcp_tool_call","server":"echo","tool":"shout","arguments":{"text":"hello mcp"},"result":null,"error":null,"status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"item_2","type":"mcp_tool_call","server":"echo","tool":"shout","arguments":{"text":"hello mcp"},"result":{"content":[{"type":"text","text":"HELLO MCP"}],"structured_content":null},"error":null,"status":"completed"}}`,
		`{"type":"item.started","item":{"id":"item_3","type":"mcp_tool_call","server":"echo","tool":"shout","arguments":{"text":"fail"},"result":null,"error":null,"status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"item_3","type":"mcp_tool_call","server":"echo","tool":"shout","arguments":{"text":"fail"},"result":{"content":[{"type":"text","text":"shout refused"}],"structured_content":null},"error":null,"status":"failed"}}`,
		`{"type":"item.completed","item":{"id":"item_4","type":"mcp_tool_call","server":"echo","tool":"gone","arguments":{},"result":null,"error":{"message":"tool not found"},"status":"failed"}}`,
		`{"type":"turn.completed","usage":{}}`,
	)
	got := codexEvents(t)
	want := []string{
		`{"type":"tool_call","id":"ws1","name":"web_search","arguments":{"query":"latest Go release"}}`,
		`{"type":"tool_result","id":"ws1","name":"web_search","result":[{"title":"Release History","url":"https://go.dev/doc/devel/release"}]}`,
		`{"type":"tool_call","id":"item_2","name":"mcp__echo__shout","arguments":{"text":"hello mcp"}}`,
		`{"type":"tool_result","id":"item_2","name":"mcp__echo__shout","result":"HELLO MCP"}`,
		`{"type":"tool_call","id":"item_3","name":"mcp__echo__shout","arguments":{"text":"fail"}}`,
		`{"type":"tool_result","id":"item_3","name":"mcp__echo__shout","result":"shout refused","is_error":true}`,
		`{"type":"tool_result","id":"item_4","name":"mcp__echo__gone","result":"tool not found","is_error":true}`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("events =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
