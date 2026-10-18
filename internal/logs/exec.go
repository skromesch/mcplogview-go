package logs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/skromesch/mcplogview-go/internal/config"
)

// localRun executes a POSIX shell script on this machine. Replaced in tests.
var localRun = func(ctx context.Context, script string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil && strings.TrimSpace(stderr.String()) != "" {
		err = fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out, err
}

// RunScript runs a shell script where the source lives: locally, or on the
// remote host through the system ssh client, and returns its stdout. The call
// is bounded by ctx and sshTimeout.
func RunScript(ctx context.Context, src config.Source, script string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, sshTimeout)
	defer cancel()
	if src.Host != "" {
		return sshRun(ctx, BuildSSHArgs(src, "sh -lc "+shellQuote(script)))
	}
	return localRun(ctx, script)
}

// ErrOutputTooLarge is returned by RunScriptLimit when stdout exceeds the limit.
var ErrOutputTooLarge = errors.New("output too large")

// maxStderrBytes bounds the stderr kept for error messages by streamRun.
const maxStderrBytes = 64 << 10

// streamRun runs name with args, streaming stdout into the given writer.
// Replaced in tests.
var streamRun = func(ctx context.Context, name string, args []string, stdout io.Writer) error {
	cmd := exec.CommandContext(ctx, name, args...)
	stderr := &discardExcess{limit: maxStderrBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// Once ssh is killed, don't wait forever for a grandchild that still
	// holds the pipes open.
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	if msg := strings.TrimSpace(stderr.buf.String()); err != nil && msg != "" {
		err = fmt.Errorf("%w: %s", err, msg)
	}
	return err
}

// discardExcess keeps the first limit bytes and silently drops the rest. It
// never fails: a failing writer would stop os/exec draining the pipe, and the
// process would block (or die of SIGPIPE) and lose its real exit status.
type discardExcess struct {
	buf   bytes.Buffer
	limit int
}

func (w *discardExcess) Write(p []byte) (int, error) {
	if room := w.limit - w.buf.Len(); room > 0 {
		w.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// capWriter keeps at most limit bytes. On the first write past the limit it
// records the overflow, calls cancel (if set) and fails the write.
type capWriter struct {
	buf    bytes.Buffer
	limit  int64
	over   bool
	cancel func()
}

func (w *capWriter) Write(p []byte) (int, error) {
	if w.over {
		return 0, ErrOutputTooLarge
	}
	room := w.limit - int64(w.buf.Len())
	if int64(len(p)) > room {
		w.buf.Write(p[:room])
		w.over = true
		if w.cancel != nil {
			w.cancel()
		}
		return int(room), ErrOutputTooLarge
	}
	return w.buf.Write(p)
}

// RunScriptLimit is RunScript with stdout bounded to limit bytes while it
// streams in: once more arrives, the command is killed and ErrOutputTooLarge
// returned, so memory use never exceeds limit regardless of the output size.
func RunScriptLimit(ctx context.Context, src config.Source, script string, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, sshTimeout)
	defer cancel()
	name, args := "sh", []string{"-c", script}
	if src.Host != "" {
		name, args = "ssh", BuildSSHArgs(src, "sh -lc "+shellQuote(script))
	}
	out := &capWriter{limit: limit, cancel: cancel}
	err := streamRun(ctx, name, args, out)
	if out.over {
		return nil, ErrOutputTooLarge
	}
	if err != nil {
		return nil, err
	}
	return out.buf.Bytes(), nil
}
