package supervisor

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/airohq/kagent-remote-harness/runner"
	"github.com/airohq/kagent-remote-harness/runner/wire"
	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"
)

type memoryEngine struct {
	*client.Client // image operations use the test Engine API
	created        chan client.ContainerCreateOptions
	removed        chan string
	started        chan string
}

func (e memoryEngine) Close() error { return nil }

func (e memoryEngine) ContainerCreate(_ context.Context, opts client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	e.created <- opts
	return client.ContainerCreateResult{ID: "owned"}, nil
}
func (e memoryEngine) ContainerAttach(context.Context, string, client.ContainerAttachOptions) (client.ContainerAttachResult, error) {
	host, worker := net.Pipe()
	go func() {
		defer worker.Close()
		decoder := json.NewDecoder(worker)
		for {
			var turn wire.TurnRequest
			if err := decoder.Decode(&turn); err != nil {
				return
			}
			e.started <- turn.Prompt
			if turn.Prompt == "wait" {
				continue
			}
			// Engine attachment frames have an eight-byte header.
			line := []byte("{\"type\":\"done\"}\n")
			frame := make([]byte, 8)
			frame[0] = 1
			binary.BigEndian.PutUint32(frame[4:], uint32(len(line)))
			if _, err := worker.Write(append(frame, line...)); err != nil {
				return
			}
		}
	}()
	return client.ContainerAttachResult{HijackedResponse: client.NewHijackedResponse(host, "")}, nil
}
func (e memoryEngine) ContainerStart(context.Context, string, client.ContainerStartOptions) (client.ContainerStartResult, error) {
	return client.ContainerStartResult{}, nil
}
func (e memoryEngine) ContainerRemove(_ context.Context, id string, _ client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	e.removed <- id
	return client.ContainerRemoveResult{}, nil
}

func TestOnDemandLifecycle(t *testing.T) {
	e := memoryEngine{created: make(chan client.ContainerCreateOptions, 4), removed: make(chan string, 4), started: make(chan string, 8)}
	c := &Container{engine: e, harness: "codex", Volume: "work", Image: "worker", Idle: 100 * time.Millisecond}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	token := strings.Repeat("a", 32)
	t.Setenv("OPENAI_API_KEY", "provider")
	t.Setenv("CODEX_API_KEY", "")
	t.Setenv("REMOTE_HARNESS_TOKEN", token)
	srv, err := runner.New(runner.Config{Harness: OnBackend(runner.Codex{}, c), Token: token, Machine: "machine", Workdir: "/work/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	request := func(ctx context.Context, path, prompt, auth string) *httptest.ResponseRecorder {
		method := "POST"
		if path == wire.InfoPath {
			method = "GET"
		}
		r := httptest.NewRequest(method, path, strings.NewReader(fmt.Sprintf(`{"prompt":%q}`, prompt))).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer "+auth)
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		return w
	}
	ctx := t.Context()
	if request(ctx, wire.InfoPath, "", token).Code != 200 || request(ctx, wire.TurnPath, "bad", "wrong").Code != 401 || request(ctx, wire.TurnPath, "", token).Code != 400 || len(e.created) != 0 {
		t.Fatal("info or invalid request started a worker")
	}
	for _, prompt := range []string{"first", "second"} {
		w := request(ctx, wire.TurnPath, prompt, token)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"done"`) {
			t.Fatal(w.Code, w.Body)
		}
		<-e.started
	}
	if len(e.created) != 1 {
		t.Fatal("warm turns created multiple workers")
	}
	opts := <-e.created
	if !slices.Equal(opts.Config.Env, []string{"OPENAI_API_KEY=provider", "CODEX_API_KEY=provider"}) || !opts.HostConfig.ReadonlyRootfs || opts.HostConfig.Mounts[0].Source != "work" {
		t.Fatal("worker configuration")
	}
	awaitRemoval := func() {
		t.Helper()
		select {
		case id := <-e.removed:
			if id != "owned" {
				t.Fatal(id)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("worker still running")
		}
	}
	awaitRemoval()
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan struct{})
	go func() { request(runCtx, wire.TurnPath, "wait", token); close(done) }()
	<-e.started
	if len(e.created) != 1 {
		t.Fatal("cold turn did not create one new worker")
	}
	if w := request(ctx, wire.TurnPath, "busy", token); w.Code != http.StatusConflict {
		t.Fatal(w.Code)
	}
	stop()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation blocked")
	}
	awaitRemoval()
}

func TestDefaultImagePullAndPin(t *testing.T) {
	var pulls, inspections atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/images/create"):
			if r.URL.Query().Get("fromImage") != strings.TrimSuffix(defaultWorkerImage, ":latest") || r.URL.Query().Get("tag") != "latest" {
				t.Errorf("unexpected pull: %s", r.URL)
			}
			if pulls.Add(1) == 1 {
				fmt.Fprintln(w, `{"errorDetail":{"message":"registry unavailable"}}`)
				return
			}
			fmt.Fprintln(w, `{"status":"Downloaded"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/images/"+defaultWorkerImage+"/json"):
			fmt.Fprintf(w, `{"Id":"image-%d"}`, inspections.Add(1))
		default:
			t.Errorf("unexpected Engine request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	engine, err := client.New(client.WithHost("tcp://"+api.Listener.Addr().String()), client.WithAPIVersion("1.54"))
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	e := memoryEngine{Client: engine, created: make(chan client.ContainerCreateOptions, 2), removed: make(chan string, 2), started: make(chan string, 2)}
	for run := 1; run <= 2; run++ {
		c := &Container{engine: e, Image: defaultWorkerImage, Idle: time.Minute}
		if run == 1 {
			err := c.start(t.Context())
			if err == nil || !strings.Contains(err.Error(), "registry unavailable") || len(e.created) != 0 || inspections.Load() != 0 {
				t.Fatalf("failed pull should stop creation and allow retry: %v", err)
			}
		}
		for range 2 {
			if err := c.start(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got := (<-e.created).Config.Image; got != fmt.Sprintf("image-%d", run) {
				t.Fatalf("run %d used image %s", run, got)
			}
			if err := c.stop(); err != nil {
				t.Fatal(err)
			}
			<-e.removed
		}
	}
	if pulls.Load() != 3 || inspections.Load() != 2 {
		t.Fatalf("pulls %d inspections %d", pulls.Load(), inspections.Load())
	}
}

func TestExplicitCodexCredential(t *testing.T) {
	t.Setenv("CODEX_API_KEY", "specific")
	t.Setenv("OPENAI_API_KEY", "general")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	if got := providerEnv("codex"); !slices.Equal(got, []string{"OPENAI_API_KEY=general", "CODEX_API_KEY=specific"}) {
		t.Error("explicit Codex key was not preserved")
	}
	if got := providerEnv("claude"); len(got) != 0 {
		t.Error("Claude received an unrelated credential")
	}
}

type loginVolumeEngine struct {
	memoryEngine
	labels map[string]string
}

func (e loginVolumeEngine) VolumeInspect(_ context.Context, _ string, _ client.VolumeInspectOptions) (client.VolumeInspectResult, error) {
	if e.labels == nil {
		return client.VolumeInspectResult{}, errdefs.ErrNotFound
	}
	return client.VolumeInspectResult{Volume: volume.Volume{Labels: e.labels}}, nil
}

func TestProviderLoginMount(t *testing.T) {
	const name = "login-test"
	for _, tt := range []struct {
		harness          string
		label            string
		mounted, wantErr bool
	}{
		{"codex", name, true, false},
		{"claude", name, true, false},
		{"codex", "foreign", false, true},
		{"codex", "", false, true},
	} {
		e := loginVolumeEngine{}
		if tt.label != "" {
			e.labels = map[string]string{loginLabel(tt.harness): tt.label}
		}
		c := Container{harness: tt.harness, loginVolume: name, engine: e}
		opts := client.ContainerCreateOptions{Config: &container.Config{}, HostConfig: &container.HostConfig{}}
		err := c.useLogin(t.Context(), &opts)
		if (err != nil) != tt.wantErr || (len(opts.HostConfig.Mounts) != 0) != tt.mounted {
			t.Fatalf("%+v: mounts=%v, err=%v", tt, opts.HostConfig.Mounts, err)
		}
		if tt.mounted {
			m := opts.HostConfig.Mounts[0]
			subpath, target := "auth", "/run/codex-login"
			if tt.harness == "claude" {
				subpath, target = "auth/.credentials.json", "/work/state/claude/.credentials.json"
			}
			if m.Source != name || m.VolumeOptions.Subpath != subpath || m.Target != target {
				t.Fatalf("unexpected login mount: %+v", m)
			}
		}
	}
}
