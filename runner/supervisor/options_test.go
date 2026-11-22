package supervisor

import (
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/airohq/kagent-remote-harness/runner/wire"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

func TestSandboxSelection(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "container"},
		{[]string{"--sandbox", "example"}, "example"},
		{[]string{"--runner", "example"}, "example"},
	} {
		var opts Options
		fs := flag.NewFlagSet("supervise", flag.ContinueOnError)
		opts.BindFlags(fs)
		if err := fs.Parse(tc.args); err != nil {
			t.Fatal(err)
		}
		if opts.Backend != tc.want {
			t.Errorf("args %v selected %q, want %q", tc.args, opts.Backend, tc.want)
		}
	}
}

func TestNativeArgsReachWorkerAndHarness(t *testing.T) {
	for _, tc := range []struct {
		name            string
		args, wantExtra []string
		mode            string
	}{
		{"codex", []string{"--sandbox", "workspace-write", "-c", "key=two words", "-m", "local-model"}, []string{"-c", "key=two words", "-m", "local-model"}, "workspace-write"},
		{"claude", []string{"--permission-mode=acceptEdits", "--settings", "", "--model=local-model"}, []string{"--settings", "", "--model=local-model"}, "acceptEdits"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var host Options
			host.BindFlags(flag.NewFlagSet("host", flag.ContinueOnError))
			host.Harness, host.CodexSandbox, host.ClaudePermissionMode = tc.name, "read-only", "plan"
			host.HarnessArgs = tc.args
			backend, err := openBackend("test-worker", host)
			if err != nil {
				t.Fatal(err)
			}
			defer backend.Close()
			argv := backend.(*Container).WorkerArgs
			var worker WorkerOptions
			fs := flag.NewFlagSet("worker", flag.ContinueOnError)
			worker.BindFlags(fs)
			if err := fs.Parse(argv[1:]); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(worker.HarnessArgs, tc.args) || fs.NArg() != 0 {
				t.Fatalf("worker argv: %q", argv)
			}
			h, err := worker.Adapter()
			if err != nil {
				t.Fatal(err)
			}
			for _, session := range []string{"", "prior-session"} {
				args, _ := h.Args(wire.TurnRequest{Model: "turn-model", ContinuationID: session}, "/work")
				var want []string
				if tc.name == "codex" {
					want = []string{"exec", "--json", "--skip-git-repo-check", "-C", "/work"}
					if session == "" {
						want = append(want, "--sandbox", tc.mode)
					} else {
						want = append(want, "-c", "sandbox_mode="+strconv.Quote(tc.mode))
					}
					want = append(want, tc.wantExtra...)
					if session != "" {
						want = append(want, "resume", session)
					}
					want = append(want, "-")
				} else {
					want = append([]string{"-p", "--output-format", "stream-json", "--verbose", "--permission-mode", tc.mode}, tc.wantExtra...)
					if session != "" {
						want = append(want, "--resume", session)
					}
				}
				if !slices.Equal(args, want) {
					t.Errorf("session %q: got %q, want %q", session, args, want)
				}
			}
		})
	}
}

func TestInvalidNativeArgs(t *testing.T) {
	for _, tc := range []struct{ harness, arg string }{
		{"codex", "--sandbox="}, {"codex", "-s"}, {"codex", "-C/tmp"}, {"codex", "--ephemeral"},
		{"claude", "--permission-mode"}, {"claude", "--output-format=text"}, {"claude", "-r"}, {"claude", "--no-session-persistence"},
	} {
		if _, _, err := nativeArgs(tc.harness, "default", []string{tc.arg}); err == nil {
			t.Errorf("accepted %s %s", tc.harness, tc.arg)
		}
	}
}

func TestWorkspaceMount(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.Symlink(".", "checkout link"); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"default", nil, ""},
		{"relative", []string{"--workspace", "checkout link"}, resolved},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var opts Options
			fs := flag.NewFlagSet("supervise", flag.ContinueOnError)
			opts.BindFlags(fs)
			if err := fs.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			opts.Image = "local/test"
			b, err := openBackend("state", opts)
			if err != nil {
				t.Fatal(err)
			}
			c := b.(*Container)
			c.engine.Close()
			e := memoryEngine{created: make(chan client.ContainerCreateOptions, 1), removed: make(chan string, 1)}
			c.engine = e
			defer c.Close()
			if err := c.start(t.Context()); err != nil {
				t.Fatal(err)
			}
			mounts := (<-e.created).HostConfig.Mounts
			if len(mounts) == 0 || mounts[0].Type != mount.TypeVolume || mounts[0].Source != "state" || mounts[0].Target != "/work" {
				t.Fatalf("state volume: %+v", mounts)
			}
			if tc.want == "" {
				if len(mounts) != 1 {
					t.Fatalf("unexpected mounts: %+v", mounts)
				}
			} else if len(mounts) != 2 || mounts[1].Type != mount.TypeBind || mounts[1].Source != tc.want || mounts[1].Target != "/work/workspace" || mounts[1].ReadOnly {
				t.Fatalf("workspace mount: %+v", mounts)
			}
		})
	}
	if err := os.WriteFile("file", nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"", "missing", "file"} {
		var opts Options
		if err := opts.SetWorkspace(invalid); err == nil {
			t.Errorf("accepted workspace %q", invalid)
		}
	}
}
