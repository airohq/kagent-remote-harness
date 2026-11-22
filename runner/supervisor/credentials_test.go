package supervisor

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCredentialSetupOwnership(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unsupported://credential-setup-test")
	for _, tc := range []struct{ harness, image, env string }{
		{"claude", defaultWorkerImage, "CLAUDE_CODE_OAUTH_TOKEN"},
		{"claude", defaultWorkerImage, "ANTHROPIC_API_KEY"},
		{"codex", defaultWorkerImage, "CODEX_API_KEY"},
		{"codex", defaultWorkerImage, "OPENAI_API_KEY"},
		{"claude", "custom/worker", ""},
	} {
		for _, key := range []string{"CODEX_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"} {
			t.Setenv(key, "")
		}
		if tc.env != "" {
			t.Setenv(tc.env, "test")
		}
		o := Options{WorkerOptions: WorkerOptions{Harness: tc.harness}, Backend: "container", Image: tc.image}
		if err := o.PrepareCredentials(t.Context(), "airo", nil, io.Discard); err != nil || o.loginVolume != "" {
			t.Fatalf("unexpected setup for %+v: %v", tc, err)
		}
	}
}

func TestSavedProviderLogin(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("exercises the Linux helper script")
	}
	for _, h := range []string{"claude", "codex"} {
		t.Run(h, func(t *testing.T) {
			auth := t.TempDir()
			file := "auth.json"
			if h == "claude" {
				file = ".credentials.json"
			}
			script := strings.NewReplacer("auth_dir=/work/auth", `auth_dir="$TEST_AUTH_DIR"`, "/run/airo-login", "$TEST_LOGIN_HOME").Replace(prepareLoginScript)
			mock := `provider() {
				case " $* " in
				  *" status "*) grep -qx saved "$TEST_LOGIN_HOME/$TEST_FILE" ;;
				  *) rm -f "$TEST_LOGIN_HOME/$TEST_FILE"
				     [ "$TEST_APPROVE" = yes ] || return 1
				     printf saved > "$TEST_LOGIN_HOME/$TEST_FILE" ;;
				esac
			}
			claude() { provider "$@"; }
			codex() { provider "$@"; }
			`
			run := func(interactive, approve string, wantStatus int) {
				t.Helper()
				cmd := exec.Command("sh", "-c", mock+script, "setup", h, interactive, "test-volume")
				cmd.Env = append(os.Environ(), "HOME="+t.TempDir(), "TEST_AUTH_DIR="+auth, "TEST_LOGIN_HOME="+t.TempDir(), "TEST_FILE="+file, "TEST_APPROVE="+approve)
				out, err := cmd.CombinedOutput()
				if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != wantStatus {
					t.Fatalf("status %v: %s", err, out)
				}
			}
			run("no", "no", 42)
			path := filepath.Join(auth, file)
			if err := os.WriteFile(path, []byte("previous"), 0600); err != nil {
				t.Fatal(err)
			}
			run("yes", "no", 1)
			if got, err := os.ReadFile(path); err != nil || string(got) != "previous" {
				t.Fatal("cancelled login replaced record", err)
			}
			run("yes", "yes", 0)
			run("no", "no", 0)
			if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0600 {
				t.Fatal("credential permissions", err)
			}
		})
	}
}
