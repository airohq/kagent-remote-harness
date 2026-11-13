package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/airohq/kagent-remote-harness/wire"
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
	wantArgs := "-p\n--output-format\nstream-json\n--verbose\n--permission-mode\nacceptEdits\n--model\nopus\n--append-system-prompt\nbe brief\n--disallowedTools=Bash\n--resume\ns1\n"
	if string(args) != wantArgs {
		t.Errorf("args =\n%s\nwant\n%s", args, wantArgs)
	}
	if stdin, _ := os.ReadFile(filepath.Join(dir, "stdin")); string(stdin) != "fix it" {
		t.Errorf("stdin = %q", stdin)
	}
	if env, _ := os.ReadFile(filepath.Join(dir, "env")); strings.Contains(string(env), testToken) {
		t.Error("the coding agent's environment holds the runner's token")
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
