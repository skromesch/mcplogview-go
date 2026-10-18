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
	// A ControlPersist master or a grandchild can keep the stderr pipe open
	// after ssh exits; do not wait for it longer than this.
	cmd.WaitDelay = 2 * time.Second
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
	if s.Multiplex {
		// Plugin sources: fail fast on a dead station (connect, or a
		// ControlMaster whose peer went away) instead of waiting sshTimeout.
		args = append(args, "-o", "ConnectTimeout=10",
			"-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=2")
		if dir := muxDirFunc(); dir != "" {
			args = append(args, "-o", "ControlMaster=auto",
				"-o", "ControlPersist=60", "-o", "ControlPath="+dir+"/%C")
		}
	}
	target := s.Host
	if s.User != "" {
		target = s.User + "@" + s.Host
	}
	return append(args, target, remoteCommand)
}

// MaxSSHBytes caps how much a single ssh call may transfer.
const MaxSSHBytes = maxSSHBytes

// RangeResult is one read of a remote file: up to maxLines whole lines
// starting at a byte offset, the offset after them, the file size and
// whether the end of the file was reached.
type RangeResult struct {
	Lines  []string
	Offset int64
	Size   int64
	EOF    bool
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

	r, err := ReadSSHRange(context.Background(), src, src.Path, requested, maxLines)
	if err != nil {
		return Chunk{}, err
	}
	return Chunk{
		Lines:  r.Lines,
		Cursor: encodeCursor(sshCursor{Source: fingerprint, Offset: r.Offset}),
		EOF:    r.EOF,
	}, nil
}

// ReadSSHRange reads up to maxLines lines of the remote file path on src's
// host, starting at byte offset (restarting at 0 when offset is past the end,
// i.e. the file was truncated or rotated). At most maxSSHBytes are
// transferred; a capped response is cut at its last newline and never
// reports EOF. The call is bounded by ctx and sshTimeout.
func ReadSSHRange(ctx context.Context, src config.Source, path string, offset int64, maxLines int) (RangeResult, error) {
	offset = max(offset, 0)
	script := fmt.Sprintf(
		`FILE=%s; `+
			`SIZE=$(wc -c < "$FILE" 2>/dev/null || echo -1); `+
			`if [ "$SIZE" -lt 0 ]; then echo "%s"; exit 12; fi; `+
			`REQ_OFFSET=%d; `+
			`if [ "$REQ_OFFSET" -gt "$SIZE" ]; then REQ_OFFSET=0; fi; `+
			`START=$((REQ_OFFSET + 1)); `+
			`printf "%s%%s\n" "$SIZE"; `+
			`tail -c +"$START" "$FILE" | head -c %d`,
		shellQuote(path), missingFileMarker, offset, sizeHeaderPrefix, maxSSHBytes,
	)
	remoteCommand := "sh -lc " + shellQuote(script)

	ctx, cancel := context.WithTimeout(ctx, sshTimeout)
	defer cancel()

	stdout, err := sshRun(ctx, BuildSSHArgs(src, remoteCommand))
	if bytes.HasPrefix(stdout, []byte(missingFileMarker)) {
		return RangeResult{}, &RemoteFileNotFoundError{Path: path}
	}
	if err != nil {
		return RangeResult{}, err
	}

	nl := bytes.IndexByte(stdout, '\n')
	if nl < 0 {
		return RangeResult{}, &InvalidSSHResponseError{Msg: "invalid ssh log response header"}
	}
	header, body := string(stdout[:nl]), stdout[nl+1:]
	if !strings.HasPrefix(header, sizeHeaderPrefix) {
		return RangeResult{}, &InvalidSSHResponseError{Msg: "missing ssh size header"}
	}
	remoteSize, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(header, sizeHeaderPrefix)), 10, 64)
	if err != nil {
		return RangeResult{}, &InvalidSSHResponseError{Msg: "missing ssh size header"}
	}

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
	return RangeResult{
		Lines:  lines,
		Offset: offset + int64(consumed),
		Size:   remoteSize,
		EOF:    eof && !capped,
	}, nil
}
