package plugin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/skromesch/mcplogview-go/internal/config"
	"github.com/skromesch/mcplogview-go/internal/logs"
)

// localBackend is an SSHBackend running everything locally: Exec runs the
// script with sh -c (ignoring the host), Tail reads a local file with the
// semantics of logs.ReadSSHRange. Scripts in stubs return their fixed output
// instead of running; hosts in down fail like an unreachable station. It backs
// the plugin test runner and the Go tests.
type localBackend struct {
	work string   // working directory of the scripts
	env  []string // extra environment of the scripts (e.g. a PATH with shims)

	mu      sync.Mutex
	calls   int
	scripts []string
	down    map[string]bool
	stubs   map[string]string // exact script -> output
}

func (b *localBackend) record(host, script string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	b.scripts = append(b.scripts, script)
	if b.down[host] {
		// deliberately without the host: the plugin/core must add it
		return errors.New("ssh: connect: Connection timed out")
	}
	return nil
}

func (b *localBackend) Exec(ctx context.Context, src config.Source, script string) ([]byte, error) {
	if err := b.record(src.Host, script); err != nil {
		return nil, err
	}
	b.mu.Lock()
	out, stubbed := b.stubs[script]
	b.mu.Unlock()
	if stubbed {
		return []byte(out), nil
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	cmd.Dir = b.work
	if len(b.env) > 0 {
		cmd.Env = append(os.Environ(), b.env...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if msg := strings.TrimSpace(stderr.String()); err != nil && msg != "" {
		err = fmt.Errorf("%w: %s", err, msg)
	}
	if err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}

func (b *localBackend) Tail(_ context.Context, src config.Source, path string, offset int64, maxLines int) (logs.RangeResult, error) {
	if err := b.record(src.Host, "tail "+path); err != nil {
		return logs.RangeResult{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return logs.RangeResult{}, &logs.RemoteFileNotFoundError{Path: path}
	}
	size := int64(len(data))
	offset = max(offset, 0)
	if offset > size {
		offset = 0
	}
	body := data[offset:]
	// like logs.extractLines: whole lines, a trailing partial line counts
	lines := make([]string, 0, maxLines)
	consumed := 0
	for len(lines) < maxLines {
		nl := bytes.IndexByte(body[consumed:], '\n')
		if nl < 0 {
			if consumed < len(body) {
				lines = append(lines, string(body[consumed:]))
				consumed = len(body)
			}
			break
		}
		lines = append(lines, string(body[consumed:consumed+nl]))
		consumed += nl + 1
	}
	return logs.RangeResult{Lines: lines, Offset: offset + int64(consumed), Size: size, EOF: consumed >= len(body)}, nil
}

// log returns the scripts sent so far (tail reads as "tail <path>").
func (b *localBackend) log() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.scripts...)
}

// stub makes script return output without running.
func (b *localBackend) stub(script, output string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stubs == nil {
		b.stubs = map[string]string{}
	}
	b.stubs[script] = output
}
