package logs

import (
	"bytes"
	"context"
	"fmt"
	"os"

	"github.com/skromesch/mcplogview-go/internal/config"
)

// Probe checks cheaply that a source is reachable and readable, without
// transferring log content. Used by `mcplogview check` and the configurator.
func Probe(src config.Source) error {
	switch src.Type {
	case config.SourceLocal:
		f, err := os.Open(src.Path)
		if err != nil {
			return err
		}
		return f.Close()
	case config.SourceSSH:
		return probeScript(src, fmt.Sprintf(
			`test -r %[1]s || { echo "%[2]smissing"; exit 12; }; echo ok`, shellQuote(src.Path), errPrefix))
	case config.SourceDocker:
		return probeScript(src, dockerScript(src.Container, "", "", true))
	}
	return fmt.Errorf("unknown source type %q", src.Type)
}

func probeScript(src config.Source, script string) error {
	ctx, cancel := context.WithTimeout(context.Background(), sshTimeout)
	defer cancel()

	out, err := RunScript(ctx, src, script)
	if bytes.HasPrefix(out, []byte(errPrefix)) {
		if src.Type == config.SourceDocker {
			return parseDockerFailure(src, out)
		}
		return &RemoteFileNotFoundError{Path: src.Path}
	}
	return err
}
