package logs

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/skromesch/mcplogview-go/internal/config"
)

const (
	missingFileMarker = "__MCPLOGVIEW_ERR__:missing"
	sizeHeaderPrefix  = "__MCPLOGVIEW_SIZE__:"
	// maxSSHBytes caps how much of the remote file is transferred per read.
	maxSSHBytes = 4 * 1024 * 1024
	sshTimeout  = 60 * time.Second
)

type sshCursor struct {
	Source string `json:"source"`
	Offset int64  `json:"offset"`
}

// sshRun executes ssh with args and returns stdout. Replaced in tests.
var sshRun = func(ctx context.Context, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "ssh", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil && strings.TrimSpace(stderr.String()) != "" {
		err = fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out, err
}

func shellQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

func sourceFingerprint(s config.Source) string {
	return fmt.Sprintf("%s@%s:%d:%s", s.User, s.Host, s.Port, s.Path)
}

// BuildSSHArgs returns the argument list passed to the ssh binary. Empty
// identity file, user and port are left to the user's ssh configuration.
func BuildSSHArgs(s config.Source, remoteCommand string) []string {
	strict := "no"
	if s.StrictHostKeyChecking {
		strict = "yes"
	}
	args := []string{}
	if s.IdentityFile != "" {
		args = append(args, "-i", s.IdentityFile)
	}
	if s.Port != 0 {
		args = append(args, "-p", strconv.Itoa(s.Port))
	}
	args = append(args, "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking="+strict)
	target := s.Host
	if s.User != "" {
		target = s.User + "@" + s.Host
	}
	return append(args, target, remoteCommand)
}

// ReadSSH tails a remote file through the system ssh client. At most
// maxSSHBytes are transferred per call; the response is cut at the last
// newline so cursors never point into the middle of a line.
func ReadSSH(src config.Source, cursor string, maxLines int) (Chunk, error) {
	if src.Port == 0 {
		src.Port = 22
	}
	fingerprint := sourceFingerprint(src)

	var requested int64
	var prev sshCursor
	if decodeCursor(cursor, &prev) == nil && prev.Source == fingerprint && prev.Offset > 0 {
		requested = prev.Offset
	}

	script := fmt.Sprintf(
		`FILE=%s; `+
			`SIZE=$(wc -c < "$FILE" 2>/dev/null || echo -1); `+
			`if [ "$SIZE" -lt 0 ]; then echo "%s"; exit 12; fi; `+
			`REQ_OFFSET=%d; `+
			`if [ "$REQ_OFFSET" -gt "$SIZE" ]; then REQ_OFFSET=0; fi; `+
			`START=$((REQ_OFFSET + 1)); `+
			`printf "%s%%s\n" "$SIZE"; `+
			`tail -c +"$START" "$FILE" | head -c %d`,
		shellQuote(src.Path), missingFileMarker, requested, sizeHeaderPrefix, maxSSHBytes,
	)
	remoteCommand := "sh -lc " + shellQuote(script)

	ctx, cancel := context.WithTimeout(context.Background(), sshTimeout)
	defer cancel()

	stdout, err := sshRun(ctx, BuildSSHArgs(src, remoteCommand))
	if bytes.HasPrefix(stdout, []byte(missingFileMarker)) {
		return Chunk{}, &RemoteFileNotFoundError{Path: src.Path}
	}
	if err != nil {
		return Chunk{}, err
	}

	nl := bytes.IndexByte(stdout, '\n')
	if nl < 0 {
		return Chunk{}, &InvalidSSHResponseError{Msg: "invalid ssh log response header"}
	}
	header, body := string(stdout[:nl]), stdout[nl+1:]
	if !strings.HasPrefix(header, sizeHeaderPrefix) {
		return Chunk{}, &InvalidSSHResponseError{Msg: "missing ssh size header"}
	}
	remoteSize, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(header, sizeHeaderPrefix)), 10, 64)
	if err != nil {
		return Chunk{}, &InvalidSSHResponseError{Msg: "missing ssh size header"}
	}

	offset := requested
	if offset > remoteSize {
		offset = 0
	}

	capped := len(body) >= maxSSHBytes
	if capped {
		if last := bytes.LastIndexByte(body, '\n'); last >= 0 {
			body = body[:last+1]
		}
	}

	lines, consumed, eof := extractLines(body, maxLines)
	return Chunk{
		Lines:  lines,
		Cursor: encodeCursor(sshCursor{Source: fingerprint, Offset: offset + int64(consumed)}),
		EOF:    eof && !capped,
	}, nil
}
