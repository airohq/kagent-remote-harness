package supervisor

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
	"github.com/muesli/cancelreader"
)

// Run credential setup only with the provider CLIs shipped in this pinned image.
const loginImage = "ghcr.io/airohq/kagent-remote-harness/runner@sha256:5272af6ec215dc0df6429eb678ab37ee2cc4e9f9a76319b20f05b5997275b2d7"

// The engine itself scopes volumes. Include the host and user configuration
// path so two clients of a shared endpoint get separate login volumes.
func loginVolumeName(application, harness string) (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	hostname, err := os.Hostname()
	if err != nil {
		return "", err
	}
	id := sha256.Sum256([]byte(hostname + "\x00" + dir))
	return fmt.Sprintf("%s-%s-login-%x", application, harness, id[:12]), nil
}

// PrepareCredentials lets the selected sandbox prepare the selected harness
// before its host registers for turns. A nil terminal permits cached login only.
// Explicit provider environment variables and caller-supplied images keep their
// own credential setup. application namespaces this client's Engine volumes.
func (o *Options) PrepareCredentials(ctx context.Context, application string, terminal *os.File, out io.Writer) error {
	o.loginVolume = ""
	if o.Backend != "container" {
		return fmt.Errorf("unknown sandbox %q: available sandboxes: container", o.Backend)
	}
	if (o.Image != defaultWorkerImage && o.Image != loginImage) || len(providerEnv(o.Harness)) != 0 {
		return nil
	}
	name, err := loginVolumeName(application, o.Harness)
	if err != nil {
		return err
	}
	engine, err := client.New(client.FromEnv)
	if err != nil {
		return err
	}
	defer engine.Close()
	if err := prepareContainerLogin(ctx, engine, loginImage, name, o.Harness, terminal, out); err != nil {
		return err
	}
	o.loginVolume = name
	return nil
}

func missingCredentials(harness string) error {
	keys := "CODEX_API_KEY or OPENAI_API_KEY"
	if harness == "claude" {
		keys = "CLAUDE_CODE_OAUTH_TOKEN or ANTHROPIC_API_KEY"
	}
	return fmt.Errorf("no saved %s login on this container engine: rerun this command in a terminal to sign in, or set %s", harness, keys)
}

// Provider CLIs keep settings and logs in tmpfs. Publish only a completed
// credential. A failed or cancelled sign-in leaves the previous login intact.
const prepareLoginScript = `set -eu
umask 077
harness=$1
interactive=$2
auth_dir=/work/auth
mkdir -p "$HOME" "$auth_dir" /run/airo-login
exec 7>>"$auth_dir/setup.lock"
flock -w 5 7 || { echo 'Another sign-in is in progress on this engine. Retry when it finishes.' >&2; exit 1; }
case "$harness" in
  claude) file=.credentials.json ;;
  codex) file=auth.json ;;
  *) exit 1 ;;
esac
if [ -s "$auth_dir/$file" ]; then
  cp "$auth_dir/$file" "/run/airo-login/$file"
  case "$harness" in
    claude) if claude auth status >/dev/null 2>&1; then exit 0; fi ;;
    codex) if codex -c 'cli_auth_credentials_store="file"' login status >/dev/null 2>&1; then exit 0; fi ;;
  esac
  rm -f "/run/airo-login/$file"
fi
[ "$interactive" = yes ] || exit 42
case "$harness" in
  claude) claude auth login --claudeai ;;
  codex) codex -c 'cli_auth_credentials_store="file"' login --device-auth ;;
esac
test -s "/run/airo-login/$file"
if [ "$harness" = codex ]; then
  exec 9>>"$auth_dir/auth.lock"
  flock -w 10 9
  exec 8>>"$auth_dir/auth.json.lock"
  flock -w 10 8
fi
stage=$(mktemp "$auth_dir/.login.XXXXXX")
trap 'rm -f "$stage"' EXIT
cp "/run/airo-login/$file" "$stage"
mv -fT "$stage" "$auth_dir/$file"
printf 'Saved %s login in container volume %s.\n' "$harness" "$3" >&2`

func prepareContainerLogin(ctx context.Context, e *client.Client, image, volume, harness string, terminal *os.File, out io.Writer) error {
	label := loginLabel(harness)
	v, err := e.VolumeInspect(ctx, volume, client.VolumeInspectOptions{})
	switch {
	case errdefs.IsNotFound(err):
		if terminal == nil {
			return missingCredentials(harness)
		}
		created, err := e.VolumeCreate(ctx, client.VolumeCreateOptions{Name: volume, Labels: map[string]string{label: volume}})
		if err != nil {
			return err
		}
		v.Volume = created.Volume
	case err != nil:
		return err
	}
	if v.Volume.Labels[label] != volume {
		return fmt.Errorf("credential volume %s already belongs to another application", volume)
	}
	if _, err := e.ImageInspect(ctx, image); errdefs.IsNotFound(err) {
		pull, err := e.ImagePull(ctx, image, client.ImagePullOptions{})
		if err != nil {
			return err
		}
		if err := pull.Wait(ctx); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	interactive := "no"
	if terminal != nil {
		interactive = "yes"
	}
	forwardInput := harness == "claude" && terminal != nil
	request := client.ContainerCreateOptions{
		Config: &container.Config{
			Image: image, User: "10001:10001",
			Entrypoint:   []string{"/bin/sh", "-ec"},
			Cmd:          []string{prepareLoginScript, "prepare-login", harness, interactive, volume},
			Env:          []string{"CODEX_HOME=/run/airo-login", "HOME=/tmp/home", "CLAUDE_CONFIG_DIR=/run/airo-login"},
			AttachStdout: true, AttachStderr: true,
			AttachStdin: forwardInput, OpenStdin: forwardInput, StdinOnce: forwardInput,
		},
		HostConfig: &container.HostConfig{
			ReadonlyRootfs: true, CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges"},
			LogConfig: container.LogConfig{Type: "none"},
			Tmpfs: map[string]string{
				"/tmp":            "rw,nosuid,nodev,mode=1777",
				"/run/airo-login": "rw,nosuid,nodev,uid=10001,gid=10001,mode=0700",
			},
			Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: volume, Target: "/work"}},
		},
	}
	if !forwardInput {
		terminal = nil
	}
	status, err := runSetup(ctx, e, request, terminal, out)
	if err != nil {
		return err
	}
	if status == 42 {
		return missingCredentials(harness)
	}
	if status != 0 {
		return fmt.Errorf("%s subscription setup exited with status %d", harness, status)
	}
	return nil
}

// runSetup attaches before start so the provider's prompt cannot be missed.
// The host owns this container until its exit status has been collected.
func runSetup(ctx context.Context, e engine, request client.ContainerCreateOptions, terminal *os.File, out io.Writer) (status int64, err error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	request.HostConfig.AutoRemove = true
	createCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	created, err := e.ContainerCreate(createCtx, request)
	if err != nil {
		return 0, err
	}
	defer func() {
		if err == nil {
			return
		} // successful wait already observed auto-removal
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, e2 := e.ContainerRemove(cleanup, created.ID, client.ContainerRemoveOptions{Force: true})
		if e2 != nil && !errdefs.IsNotFound(e2) {
			err = errors.Join(err, e2)
		}
	}()
	stream, err := e.ContainerAttach(ctx, created.ID, client.ContainerAttachOptions{Stream: true, Stdout: true, Stderr: true, Stdin: terminal != nil})
	if err != nil {
		return 0, err
	}
	defer stream.Close()
	// Register before start: a quick status check can exit and auto-remove
	// before its output stream closes. Always consume the SDK wait channels.
	waitCtx, cancelWait := context.WithCancel(ctx)
	defer cancelWait()
	wait := e.ContainerWait(waitCtx, created.ID, client.ContainerWaitOptions{Condition: container.WaitConditionRemoved})
	type exit struct {
		code int64
		err  error
	}
	exited := make(chan exit, 1)
	go func() {
		select {
		case result := <-wait.Result:
			exited <- exit{code: result.StatusCode}
		case err := <-wait.Error:
			exited <- exit{err: err}
		}
	}()
	if _, err := e.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return 0, err
	}
	if terminal != nil {
		reader, err := cancelreader.NewReader(terminal)
		if err != nil {
			return 0, err
		}
		inputDone := make(chan struct{})
		go func() {
			defer close(inputDone)
			_, _ = io.Copy(stream.Conn, reader)
			_ = stream.CloseWrite()
		}()
		defer func() {
			reader.Cancel()
			_ = stream.CloseWrite()
			<-inputDone
			_ = reader.Close()
		}()
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { stream.Close(); close(done) })
	_, err = stdcopy.StdCopy(out, out, stream.Reader)
	if !stop() {
		<-done
	}
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if err != nil {
		return 0, err
	}
	result := <-exited
	return result.code, result.err
}
