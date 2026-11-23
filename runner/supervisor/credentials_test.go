package supervisor

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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

// loginScript runs a login script against a test login volume, with the
// provider CLIs replaced by mock.
func loginScript(t *testing.T, auth, script, mock string, env []string, args ...string) (int, string) {
	t.Helper()
	script = strings.NewReplacer("auth_dir=/work/auth", `auth_dir="$TEST_AUTH_DIR"`, "/run/airo-login", "$TEST_LOGIN_HOME").Replace(script)
	cmd := exec.Command("sh", append([]string{"-c", mock + script, "login"}, args...)...)
	cmd.Env = append(os.Environ(), append(env, "HOME="+t.TempDir(), "TEST_AUTH_DIR="+auth, "TEST_LOGIN_HOME="+t.TempDir())...)
	out, _ := cmd.CombinedOutput()
	if cmd.ProcessState == nil {
		t.Fatalf("login script did not run: %s", out)
	}
	return cmd.ProcessState.ExitCode(), string(out)
}

func TestLoginCheck(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("exercises the Linux helper script")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("needs node, as the worker image has")
	}
	soon := time.Now().Add(time.Hour).UnixMilli()
	live := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","expiresAt":%d}}`, soon)
	// Claude Code writes this after a refresh fails, and still reports it as
	// signed in.
	wiped := `{"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0}}`
	expiredAccess := `{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","expiresAt":1}}`
	signedIn := `{"loggedIn":true,"loginExpiresAt":null}`
	mock := `claude() { [ "$1 $2" = "auth status" ] && echo "$TEST_STATUS"; }
	codex() { grep -qx saved "$TEST_LOGIN_HOME/auth.json"; }
	`
	for _, tc := range []struct {
		name, harness, saved, status string
		want                         int
	}{
		{"no claude login", "claude", "", signedIn, loginMissing},
		{"live claude login", "claude", live, signedIn, 0},
		{"refreshable claude login", "claude", expiredAccess, signedIn, 0},
		{"wiped claude login", "claude", wiped, signedIn, loginExpired},
		{"claude signed out", "claude", live, `{"loggedIn":false}`, loginExpired},
		{"claude sign-in ended", "claude", live, `{"loggedIn":true,"loginExpiresAt":"2020-01-01T00:00:00Z"}`, loginExpired},
		{"claude sign-in ends later", "claude", live, `{"loggedIn":true,"loginExpiresAt":"2999-01-01T00:00:00Z"}`, 0},
		{"no codex login", "codex", "", "", loginMissing},
		{"codex login", "codex", "saved", "", 0},
		{"codex signed out", "codex", "stale", "", loginExpired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := t.TempDir()
			file := map[string]string{"claude": ".credentials.json", "codex": "auth.json"}[tc.harness]
			if tc.saved != "" {
				if err := os.WriteFile(filepath.Join(auth, file), []byte(tc.saved), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			status, out := loginScript(t, auth, checkLoginScript, mock, []string{"TEST_STATUS=" + tc.status}, tc.harness, "test-volume")
			if status != tc.want {
				t.Fatalf("status %d, want %d: %s", status, tc.want, out)
			}
			if got, _ := os.ReadFile(filepath.Join(auth, file)); string(got) != tc.saved {
				t.Fatalf("the check changed the saved login to %q", got)
			}
		})
	}
}

func TestSignIn(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("exercises the Linux helper script")
	}
	for _, h := range []string{"claude", "codex"} {
		t.Run(h, func(t *testing.T) {
			auth := t.TempDir()
			file := map[string]string{"claude": ".credentials.json", "codex": "auth.json"}[h]
			mock := `provider() {
				rm -f "$TEST_LOGIN_HOME/$TEST_FILE"
				[ "$TEST_APPROVE" = yes ] || return 1
				printf saved > "$TEST_LOGIN_HOME/$TEST_FILE"
			}
			claude() { provider "$@"; }
			codex() { provider "$@"; }
			`
			path := filepath.Join(auth, file)
			if err := os.WriteFile(path, []byte("previous"), 0o600); err != nil {
				t.Fatal(err)
			}
			if status, out := loginScript(t, auth, signInScript, mock, []string{"TEST_FILE=" + file, "TEST_APPROVE=no"}, h, "test-volume"); status != 1 {
				t.Fatalf("cancelled sign-in: status %d: %s", status, out)
			}
			if got, err := os.ReadFile(path); err != nil || string(got) != "previous" {
				t.Fatal("cancelled login replaced record", err)
			}
			if status, out := loginScript(t, auth, signInScript, mock, []string{"TEST_FILE=" + file, "TEST_APPROVE=yes"}, h, "test-volume"); status != 0 {
				t.Fatalf("sign-in: status %d: %s", status, out)
			}
			if got, err := os.ReadFile(path); err != nil || string(got) != "saved" {
				t.Fatal("sign-in did not save the login", err)
			}
			if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
				t.Fatal("credential permissions", err)
			}
		})
	}
}

// TestClaudeLoginSync runs the worker's Claude wrapper with a mock Claude
// that reads and refreshes its login.
func TestClaudeLoginSync(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("exercises the Linux helper script")
	}
	login, bin, config, real := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	// The mock prints the login it got, then saves $TEST_REFRESH as its
	// refreshed login, replacing the file as Claude Code does.
	mock := `#!/bin/sh
cat "$CLAUDE_CONFIG_DIR/.credentials.json"
if [ -n "${TEST_REFRESH:-}" ]; then
  printf %s "$TEST_REFRESH" > "$CLAUDE_CONFIG_DIR/.new"
  mv "$CLAUDE_CONFIG_DIR/.new" "$CLAUDE_CONFIG_DIR/.credentials.json"
fi
exit "${TEST_EXIT:-0}"
`
	if err := os.WriteFile(filepath.Join(real, "claude"), []byte(mock), 0o755); err != nil {
		t.Fatal(err)
	}
	entrypoint := strings.NewReplacer("/run/claude-login", login, "/run/claude-bin", bin, `exec remote-harness "$@"`, `exec claude "$@"`).Replace(claudeLoginEntrypoint)
	run := func(refresh string, exit int) string {
		t.Helper()
		cmd := exec.Command("sh", "-ec", entrypoint, "claude-login")
		cmd.Env = append(os.Environ(), "PATH="+real+string(os.PathListSeparator)+os.Getenv("PATH"),
			"CLAUDE_CONFIG_DIR="+config, "TEST_REFRESH="+refresh, fmt.Sprintf("TEST_EXIT=%d", exit))
		out, err := cmd.Output()
		if code := cmd.ProcessState.ExitCode(); code != exit {
			t.Fatalf("exit %d, want %d: %v", code, exit, err)
		}
		return string(out)
	}
	saved := func() string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(login, ".credentials.json"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	setSaved := func(s string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(login, ".credentials.json"), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	setSaved("A")
	if got := run("", 0); got != "A" {
		t.Fatalf("a new workspace got login %q, want the saved A", got)
	}
	if got := run("B", 0); got != "A" || saved() != "B" {
		t.Fatalf("got %q and saved %q, want A and the refreshed B", got, saved())
	}
	// A failed run still saves its refresh, as Claude Code's exit does not
	// undo it.
	if run("C", 3); saved() != "C" {
		t.Fatalf("saved %q after a failed run, want the refreshed C", saved())
	}
	// A run killed before it saved: the next run saves its refresh first.
	cmd := exec.Command(filepath.Join(real, "claude"))
	cmd.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+config, "TEST_REFRESH=D")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if got := run("", 0); got != "D" || saved() != "D" {
		t.Fatalf("got %q and saved %q, want the unsaved refresh D", got, saved())
	}
	// Another workspace or a new sign-in changed the saved login.
	setSaved("E")
	if got := run("", 0); got != "E" {
		t.Fatalf("got %q, want the newly saved E", got)
	}
	// Both changed: the saved login wins.
	cmd = exec.Command(filepath.Join(real, "claude"))
	cmd.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+config, "TEST_REFRESH=F")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	setSaved("G")
	if got := run("", 0); got != "G" || saved() != "G" {
		t.Fatalf("got %q and saved %q, want the saved G", got, saved())
	}
	if st, err := os.Stat(filepath.Join(login, ".credentials.json")); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatal("saved login permissions", err)
	}
}
