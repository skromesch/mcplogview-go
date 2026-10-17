package logs

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

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

// runScript runs a shell script where the source lives: locally, or on the
// remote host through the system ssh client.
func runScript(ctx context.Context, src config.Source, script string) ([]byte, error) {
	if src.Host != "" {
		return sshRun(ctx, BuildSSHArgs(src, "sh -lc "+shellQuote(script)))
	}
	return localRun(ctx, script)
}
