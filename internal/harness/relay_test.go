package harness

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	kagentconfig "github.com/kagent-dev/kagent/go/adk/pkg/config"
	contract "github.com/kagent-dev/kagent/go/api/bootstrap"
	configv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/config/v1"
	"github.com/kagent-dev/kagent/go/harness/runtime"

	"github.com/airohq/kagent-remote-harness/runner"
	"github.com/airohq/kagent-remote-harness/wire"
)

const testToken = "0123456789abcdef0123456789abcdef"

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type sink struct {
	sessions []string
	text     strings.Builder
	tools    []string
}

func (s *sink) SessionStarted(e runtime.SessionStarted) error {
	s.sessions = append(s.sessions, e.ContinuationID)
	return nil
}
func (s *sink) TextDelta(e runtime.TextDelta) error { s.text.WriteString(e.Text); return nil }
func (s *sink) ToolCall(e runtime.ToolCall) error {
	s.tools = append(s.tools, "call:"+e.Name)
	return nil
}
func (s *sink) ToolResult(e runtime.ToolResult) error {
	s.tools = append(s.tools, "result:"+e.Name)
	return nil
}
func (s *sink) ModelCall(runtime.ModelCall) {}

// tokenGateway stands in for kagent's egress gateway: it replaces the
// harness's placeholder token with the real one.
func tokenGateway(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+placeholderToken {
			http.Error(w, "no placeholder", http.StatusBadRequest)
			return
		}
		r.Header.Set("Authorization", "Bearer "+testToken)
		next.ServeHTTP(w, r)
	})
}

// startRunner serves a real Runner whose claude is a script.
func startRunner(t *testing.T, machine string) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
printf '%s\n' "$@" > "` + dir + `/args"
cat > /dev/null
echo '{"type":"system","subtype":"init","session_id":"s1"}'
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"done it"},{"type":"tool_use","id":"t1","name":"Edit","input":{}}]}}'
echo '{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}'
echo '{"type":"result","subtype":"success","is_error":false}'
`
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	r, err := runner.New(runner.Config{Harness: runner.Claude{PermissionMode: "acceptEdits"}, Token: testToken, Machine: machine, Workdir: t.TempDir(), Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(tokenGateway(r))
	t.Cleanup(srv.Close)
	return srv
}

func relay(url string, wait time.Duration) *Relay {
	return &Relay{URL: url, Instructions: "be brief", Model: "opus", Wait: wait, HTTP: &http.Client{}, Logger: quiet}
}

func TestRelayRunsATurnOnTheRunner(t *testing.T) {
	srv := startRunner(t, "johndoe-mbp")
	var s sink
	out, err := relay(srv.URL, time.Second).Run(t.Context(), runtime.Turn{Prompt: "fix it"}, &s)
	if err != nil || out.Failure != nil {
		t.Fatalf("Run = %+v, %v", out.Failure, err)
	}
	if len(s.sessions) != 1 || s.sessions[0] != "s1@johndoe-mbp" {
		t.Errorf("sessions = %v", s.sessions)
	}
	if s.text.String() != "done it" || strings.Join(s.tools, ",") != "call:Edit,result:Edit" {
		t.Errorf("text = %q, tools = %v", s.text.String(), s.tools)
	}

	// A later turn on the same machine resumes the session.
	s = sink{}
	out, err = relay(srv.URL, time.Second).Run(t.Context(), runtime.Turn{Prompt: "more", ContinuationID: "s1@johndoe-mbp"}, &s)
	if err != nil || out.Failure != nil {
		t.Fatalf("Run = %+v, %v", out.Failure, err)
	}
}

func TestRelayFailsATurnThatReachesAnotherMachine(t *testing.T) {
	srv := startRunner(t, "other-machine")
	var s sink
	out, err := relay(srv.URL, time.Second).Run(t.Context(), runtime.Turn{Prompt: "more", ContinuationID: "s1@johndoe-mbp"}, &s)
	if err != nil || out.Failure == nil || !strings.Contains(out.Failure.Message, `started on machine "johndoe-mbp"`) {
		t.Fatalf("Run = %+v, %v", out.Failure, err)
	}
	if len(s.sessions) != 0 {
		t.Errorf("sessions = %v", s.sessions)
	}
}

// A tunnel without a runner answers without the protocol header; the turn
// waits, then fails with a message that the runner is offline.
func TestRelayWaitsForAnOfflineRunner(t *testing.T) {
	var calls atomic.Int32
	offline := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "ERR_NGROK_3200: endpoint offline", http.StatusNotFound)
	}))
	defer offline.Close()
	out, err := relay(offline.URL, 1500*time.Millisecond).Run(t.Context(), runtime.Turn{Prompt: "x"}, &sink{})
	if err != nil || out.Failure == nil || !strings.Contains(out.Failure.Message, "offline or busy") {
		t.Fatalf("Run = %+v, %v", out.Failure, err)
	}
	if calls.Load() < 2 {
		t.Errorf("the harness tried %d times, want a retry", calls.Load())
	}
}

func TestRelayRetriesABusyRunner(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(wire.ProtocolHeader, wire.Version)
		if calls.Add(1) == 1 {
			http.Error(w, "busy", http.StatusConflict)
			return
		}
		_ = json.NewEncoder(w).Encode(wire.Event{Type: wire.EventSession, ContinuationID: "s1", Machine: "m"})
		_ = json.NewEncoder(w).Encode(wire.Event{Type: wire.EventDone})
	}))
	defer srv.Close()
	out, err := relay(srv.URL, 10*time.Second).Run(t.Context(), runtime.Turn{Prompt: "x"}, &sink{})
	if err != nil || out.Failure != nil {
		t.Fatalf("Run = %+v, %v", out.Failure, err)
	}
}

func TestRelayReportsTheRunnersRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(wire.ProtocolHeader, wire.Version)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()
	out, err := relay(srv.URL, time.Minute).Run(t.Context(), runtime.Turn{Prompt: "x"}, &sink{})
	if err != nil || out.Failure == nil || !strings.Contains(out.Failure.Message, "401") {
		t.Fatalf("Run = %+v, %v", out.Failure, err)
	}
}

func TestRelayStopsWhenCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(wire.ProtocolHeader, wire.Version)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if _, err := relay(srv.URL, time.Minute).Run(ctx, runtime.Turn{Prompt: "x"}, &sink{}); err == nil {
		t.Fatal("Run returned no error after its context ended")
	}
}

func TestContinuationIDs(t *testing.T) {
	id := JoinContinuation("5f1c-uuid", "johndoe-mbp")
	if s, m := SplitContinuation(id); s != "5f1c-uuid" || m != "johndoe-mbp" {
		t.Errorf("split = %q, %q", s, m)
	}
	if ValidateContinuation(id) != nil || ValidateContinuation("no-machine") == nil || ValidateContinuation("a b@m") == nil {
		t.Error("ValidateContinuation accepts or rejects the wrong IDs")
	}
}

// Checks that the harness finds its settings in the bootstrap bundle kagent
// builds for a BYO harness, once the bundle is on the durable volume.
func TestLoadBundleReadsKagentBYOConfig(t *testing.T) {
	config, err := kagentconfig.Marshal(&configv1.ADKConfig{
		Runtime: &configv1.ADKRuntimeConfig{ControlPlaneUrl: "http://kagent-controller.kagent.svc.cluster.local:8083"},
		Agent: &configv1.ADKAgentConfig{
			Name:        "my_laptop",
			Instruction: "You work in the repository.",
			Model: &configv1.ADKModelConfig{
				Model:    "opus",
				Provider: &configv1.ADKModelConfig_Anthropic{Anthropic: &configv1.ADKAnthropic{}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	revision := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	bundle, err := contract.NewBundle(revision, config, []byte(`{"name":"my_laptop"}`))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	raw, err := proto.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bundle.pb"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	descriptor, err := json.Marshal(contract.Descriptor{
		FormatVersion:   contract.FormatVersion,
		ControlPlaneURL: "http://kagent-controller.kagent.svc.cluster.local:8083",
		Revision:        revision,
		SHA256:          bundle.Sha256,
	})
	if err != nil {
		t.Fatal(err)
	}
	getenv := func(key string) string {
		if key == contract.Environment {
			return string(descriptor)
		}
		return ""
	}

	cfg, card, err := LoadBundle(context.Background(), getenv, dir)
	if err != nil {
		t.Fatal(err)
	}
	if card.Name != "my_laptop" {
		t.Errorf("card name = %q", card.Name)
	}
	if got := cfg.GetAgent().GetModel().GetModel(); got != "opus" {
		t.Errorf("model = %q", got)
	}
	if got := cfg.GetAgent().GetInstruction(); got != "You work in the repository." {
		t.Errorf("instruction = %q", got)
	}
}
