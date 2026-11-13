package runner

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	// maxLineBytes limits one line of a coding agent's output. A longer line
	// is dropped; the event it carried is lost, the turn goes on.
	maxLineBytes = 64 << 20
	// stderrTailBytes is how much of a coding agent's stderr a failure quotes.
	stderrTailBytes = 2 << 10
	// waitDelay is how long the runner waits for the coding agent's output
	// after it exits, such as when a process it started keeps stdout open.
	waitDelay = 5 * time.Second
)

// Process runs coding agent CLIs for one turn.
type Process struct {
	// Dir is the working directory.
	Dir string
	// Env is the environment, without the runner's token.
	Env        []string
	Logger     *slog.Logger
	LogContent bool
}

// Stream runs name with args, writes stdin to it, and calls onLine for each
// line of its stdout. The command runs in its own process group: when ctx
// ends, or when the command exits, the runner kills the whole group, so no
// process it started outlives the turn.
func (p Process) Stream(ctx context.Context, name string, args []string, stdin string, onLine func([]byte)) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = p.Dir
	cmd.Env = p.Env
	cmd.Stdin = strings.NewReader(stdin)
	lines := &lineWriter{onLine: onLine}
	stderr := &tailWriter{max: stderrTailBytes}
	cmd.Stdout = lines
	cmd.Stderr = stderr
	cmd.WaitDelay = waitDelay
	setProcessGroup(cmd)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", name, err)
	}
	stop := context.AfterFunc(ctx, func() { killGroup(cmd) })
	err := cmd.Wait()
	stop()
	killGroup(cmd)
	lines.flush()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		if tail := strings.TrimSpace(stderr.String()); tail != "" {
			return fmt.Errorf("%s: %w: %s", name, err, tail)
		}
		return fmt.Errorf("%s: %w", name, err)
	}
	if lines.dropped > 0 {
		p.Logger.Warn("dropped output lines over 64 MiB", "command", name, "lines", lines.dropped)
	}
	return nil
}

// lineWriter splits what it is written into lines.
type lineWriter struct {
	mu       sync.Mutex
	buf      []byte
	skipping bool
	dropped  int
	onLine   func([]byte)
}

func (l *lineWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			l.add(p)
			break
		}
		l.add(p[:i])
		if !l.skipping {
			l.onLine(l.buf)
		}
		l.buf, l.skipping = l.buf[:0], false
		p = p[i+1:]
	}
	return n, nil
}

func (l *lineWriter) add(p []byte) {
	if l.skipping {
		return
	}
	if len(l.buf)+len(p) > maxLineBytes {
		l.buf, l.skipping = l.buf[:0], true
		l.dropped++
		return
	}
	l.buf = append(l.buf, p...)
}

func (l *lineWriter) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buf) > 0 && !l.skipping {
		l.onLine(l.buf)
	}
	l.buf = l.buf[:0]
}

// tailWriter keeps the last max bytes written to it.
type tailWriter struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailWriter) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
