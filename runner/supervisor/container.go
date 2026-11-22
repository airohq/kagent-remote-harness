package supervisor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"sync"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/airohq/kagent-remote-harness/runner"
	"github.com/airohq/kagent-remote-harness/runner/wire"
)

type engine interface {
	io.Closer
	VolumeInspect(context.Context, string, client.VolumeInspectOptions) (client.VolumeInspectResult, error)
	ImagePull(context.Context, string, client.ImagePullOptions) (client.ImagePullResponse, error)
	ImageInspect(context.Context, string, ...client.ImageInspectOption) (client.ImageInspectResult, error)
	ContainerCreate(context.Context, client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	ContainerAttach(context.Context, string, client.ContainerAttachOptions) (client.ContainerAttachResult, error)
	ContainerStart(context.Context, string, client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerRemove(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
}

// Container keeps one worker warm between turns over the Engine API.
type Container struct {
	harness          string
	codexLoginVolume string
	engine           engine
	Image, Volume    string
	WorkerArgs       []string
	Idle             time.Duration

	mu sync.Mutex
	// Pinned across idle worker removal for the lifetime of this supervisor.
	pulledImageID string
	id            string
	stream        *client.ContainerAttachResult
	output        *io.PipeReader
	events        *bufio.Scanner
	timer         *time.Timer
}

// NewContainer opens the configured Engine API client. The worker is created
// on the first turn.
func NewContainer(harness, volume, image string, idle time.Duration, workerArgs []string) (*Container, error) {
	if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,120}$`).MatchString(volume) {
		return nil, errors.New("set --name to a volume name of up to 121 letters, digits, dots, underscores or hyphens")
	}
	engine, err := client.New(client.FromEnv)
	if err != nil {
		return nil, err
	}
	return &Container{harness: harness, engine: engine, Volume: volume, Image: image, Idle: idle, WorkerArgs: workerArgs}, nil
}

func providerEnv(harness string) []string {
	var env []string
	keys := []string{"OPENAI_API_KEY"}
	if harness == "claude" {
		keys = []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"}
	}
	for _, key := range keys {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	// Headless Codex reads CODEX_API_KEY. Keep OPENAI_API_KEY available to
	// existing images and prefer an explicit Codex credential when provided.
	if harness == "codex" {
		key := os.Getenv("CODEX_API_KEY")
		if key == "" {
			key = os.Getenv("OPENAI_API_KEY")
		}
		if key != "" {
			env = append(env, "CODEX_API_KEY="+key)
		}
	}
	return env
}

// workerImage runs under mu. Only the published default tracks a registry tag.
func (c *Container) workerImage(ctx context.Context) (string, error) {
	if c.Image != defaultWorkerImage {
		return c.Image, nil
	}
	if c.pulledImageID != "" {
		return c.pulledImageID, nil
	}
	pull, err := c.engine.ImagePull(ctx, c.Image, client.ImagePullOptions{})
	if err == nil {
		err = pull.Wait(ctx) // consumes and closes the Engine's progress stream
	}
	if err != nil {
		return "", fmt.Errorf("pull default worker image %s: %w", c.Image, err)
	}
	image, err := c.engine.ImageInspect(ctx, c.Image)
	if err != nil {
		return "", fmt.Errorf("inspect pulled worker image %s: %w", c.Image, err)
	}
	if image.ID == "" {
		return "", fmt.Errorf("engine returned no image ID for %s", c.Image)
	}
	c.pulledImageID = image.ID
	return image.ID, nil
}

// CodexLoginLabel identifies an opt-in credential volume. Its value is the volume name.
const CodexLoginLabel = "remote-harness.dev/codex-login"

const codexLoginEntrypoint = `set -eu
test -s /run/codex-login/auth.json || { echo 'Codex subscription login missing; sign in again' >&2; exit 1; }
mkdir -p "$CODEX_HOME"
for file in auth.json auth.lock; do
	link="$CODEX_HOME/$file"
	target="/run/codex-login/$file"
	if [ -L "$link" ] && [ "$(readlink "$link")" = "$target" ]; then continue; fi
	if [ "$file" = auth.lock ] && [ -f "$link" ] && [ ! -L "$link" ]; then rm "$link"; fi
	if [ -e "$link" ] || [ -L "$link" ]; then echo "Move the existing credential at $link before using the shared Codex login" >&2; exit 1; fi
	ln -s "$target" "$link"
done
exec remote-harness "$@"`

func (c *Container) useCodexLogin(ctx context.Context, opts *client.ContainerCreateOptions) error {
	name := c.codexLoginVolume
	if name == "" || c.harness != "codex" {
		return nil
	}
	v, err := c.engine.VolumeInspect(ctx, name, client.VolumeInspectOptions{})
	if errdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect Codex credential volume: %w", err)
	}
	if v.Volume.Labels[CodexLoginLabel] != name {
		return fmt.Errorf("volume %s is not a Codex credential volume", name)
	}
	// Codex refreshes credentials by atomic replacement and coordinates writes
	// with file locks. Share only the dedicated credential directory.
	opts.HostConfig.Mounts = append(opts.HostConfig.Mounts, mount.Mount{
		Type: mount.TypeVolume, Source: name, Target: "/run/codex-login",
		VolumeOptions: &mount.VolumeOptions{NoCopy: true, Subpath: "auth"},
	})
	opts.Config.Entrypoint = []string{"/bin/sh", "-ec", codexLoginEntrypoint, "codex-login"}
	return nil
}

func (c *Container) start(ctx context.Context) error {
	image, err := c.workerImage(ctx)
	if err != nil {
		return err
	}
	env := providerEnv(c.harness)
	pids := int64(512)
	options := client.ContainerCreateOptions{
		Name: c.Volume,
		Config: &container.Config{
			Image: image, User: "10001:10001", Cmd: c.WorkerArgs, Env: env,
			OpenStdin: true, StdinOnce: true,
			AttachStdin: true, AttachStdout: true, AttachStderr: true,
		},
		HostConfig: &container.HostConfig{
			AutoRemove:     true,
			LogConfig:      container.LogConfig{Type: "none"},
			ReadonlyRootfs: true,
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges"},
			Tmpfs:          map[string]string{"/tmp": "rw,nosuid,nodev,mode=1777"},
			Mounts:         []mount.Mount{{Type: mount.TypeVolume, Source: c.Volume, Target: "/work"}},
			Resources:      container.Resources{Memory: 4 << 30, NanoCPUs: 2e9, PidsLimit: &pids},
		},
	}
	if err := c.useCodexLogin(ctx, &options); err != nil {
		return err
	}

	if err := ctx.Err(); err != nil {
		return err
	}
	// Collect the new ID even if the request is canceled during creation;
	// the remaining steps observe cancellation and remove the owned ID.
	createCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	created, err := c.engine.ContainerCreate(createCtx, options)
	if err != nil {
		return fmt.Errorf("create runner from %s (load the worker image into this engine first): %w", c.Image, err)
	}
	c.id = created.ID
	attached, err := c.engine.ContainerAttach(ctx, c.id, client.ContainerAttachOptions{Stream: true, Stdin: true, Stdout: true, Stderr: true})
	if err != nil {
		return err
	}
	c.stream = &attached

	reader, writer := io.Pipe()
	c.output = reader
	c.events = bufio.NewScanner(reader)
	c.events.Buffer(make([]byte, 64<<10), wire.MaxEventBytes+1)
	go func() {
		_, err := stdcopy.StdCopy(writer, os.Stderr, attached.Reader)
		writer.CloseWithError(err)
	}()
	_, err = c.engine.ContainerStart(ctx, c.id, client.ContainerStartOptions{})
	return err
}

func (c *Container) RunTurn(ctx context.Context, turn wire.TurnRequest, emit runner.Emit) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	if c.stream == nil {
		if c.id != "" {
			if err := c.stop(); err != nil {
				return err
			}
		}
		if err := c.start(ctx); err != nil {
			return errors.Join(err, c.stop())
		}
	}
	// A canceled request must interrupt an in-flight read or write. Closing
	// the attachment wakes both, then stop removes exactly the owned container.
	stream := c.stream
	finished := make(chan struct{})
	stopCancel := context.AfterFunc(ctx, func() { stream.Close(); close(finished) })
	err, terminal := c.exchange(turn, emit)
	if !stopCancel() {
		<-finished
	}
	if !terminal || ctx.Err() != nil {
		return errors.Join(ctx.Err(), err, c.stop())
	}
	var timer *time.Timer
	timer = time.AfterFunc(c.Idle, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.timer == timer {
			if err := c.stop(); err != nil {
				slog.Error("stop idle runner", "error", err)
			}
		}
	})
	c.timer = timer
	return err
}

func (c *Container) exchange(turn wire.TurnRequest, emit runner.Emit) (error, bool) {
	if err := json.NewEncoder(c.stream.Conn).Encode(turn); err != nil {
		return err, false
	}
	for c.events.Scan() {
		var event wire.Event
		if err := json.Unmarshal(c.events.Bytes(), &event); err != nil {
			return err, false
		}
		switch event.Type {
		case wire.EventDone:
			return nil, true
		case wire.EventFailure:
			return errors.New(event.Message), true
		default:
			emit(event)
		}
	}
	if err := c.events.Err(); err != nil {
		return err, false
	}
	return errors.New("runner image closed its worker stream before the turn finished; use an image with the worker command"), false
}

func (c *Container) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return errors.Join(c.stop(), c.engine.Close())
}

func (c *Container) stop() error {
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	if c.output != nil {
		c.output.Close()
		c.output = nil
	}
	if c.stream != nil {
		c.stream.Close()
		c.stream = nil
	}
	if c.id == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := c.engine.ContainerRemove(ctx, c.id, client.ContainerRemoveOptions{Force: true})
	if err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("remove runner %s: %w", c.id, err)
	}
	c.id = ""
	return nil
}
