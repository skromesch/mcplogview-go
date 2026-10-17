package logs

import (
	"bytes"
	"context"
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	"github.com/skromesch/mcplogview-go/internal/config"
)

const (
	dockerIDPrefix = "__MCPLOGVIEW_DOCKER_ID__:"
	errPrefix      = "__MCPLOGVIEW_ERR__:"
)

// ContainerNotFoundError is returned when the container does not exist.
type ContainerNotFoundError struct{ Name string }

func (e *ContainerNotFoundError) Error() string {
	return fmt.Sprintf("container not found: %s", e.Name)
}

// DockerError wraps a failure reported by the docker CLI or its absence.
type DockerError struct{ Msg string }

func (e *DockerError) Error() string { return e.Msg }

// dockerCursor continues a container log by timestamp.
//
// The Docker daemon stamps stdout and stderr lines in separate goroutines, so a
// line can land in the log *after* a line with a newer timestamp. Resuming
// strictly after the last timestamp would silently lose such lines. Instead a
// read restarts resumeOverlap earlier and drops the lines it has already
// delivered, recognised by Recent (hashes of the last recentMax lines).
// Cost: a very chatty log may repeat lines older than recentMax in the overlap.
// ID ties the cursor to one container incarnation (recreation resets it).
type dockerCursor struct {
	Source string `json:"source"`
	ID     string `json:"id"`
	TS     string `json:"ts"`
	Recent string `json:"recent"`
}

const (
	resumeOverlap = 250 * time.Millisecond
	recentMax     = 64
	hashLen       = 8
)

func lineHash(line string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(line))
	return fmt.Sprintf("%08x", h.Sum32())
}

func dockerFingerprint(s config.Source) string {
	return fmt.Sprintf("docker:%s:%s", s.Host, s.Container)
}

// dockerScript inspects the container and (unless probeOnly) streams its logs
// with timestamps, stdout and stderr merged, capped at maxSSHBytes.
func dockerScript(container, since, expectID string, probeOnly bool) string {
	var b strings.Builder
	b.WriteString(`command -v docker >/dev/null 2>&1 || { echo "` + errPrefix + `nodocker"; exit 13; }; `)
	fmt.Fprintf(&b, `OUT=$(docker container inspect --format '{{.Id}}' %s 2>&1) || { printf '%sinspect:%%s\n' "$OUT"; exit 12; }; `, shellQuote(container), errPrefix)
	fmt.Fprintf(&b, `printf '%s%%s\n' "$OUT"; `, dockerIDPrefix)
	if probeOnly {
		return b.String()
	}
	fmt.Fprintf(&b, `SINCE=%s; EXPECT=%s; `, shellQuote(since), shellQuote(expectID))
	b.WriteString(`if [ -n "$EXPECT" ] && [ "$EXPECT" != "$OUT" ]; then SINCE=''; fi; `)
	fmt.Fprintf(&b, `if [ -n "$SINCE" ]; then docker logs --timestamps --since "$SINCE" %[1]s 2>&1; else docker logs --timestamps %[1]s 2>&1; fi | head -c %[2]d`,
		shellQuote(container), maxSSHBytes)
	return b.String()
}

func parseDockerFailure(src config.Source, out []byte) error {
	// Docker sometimes emits a blank line before its message; keep all of it.
	msg := strings.Join(strings.Fields(strings.TrimPrefix(string(out), errPrefix)), " ")
	switch {
	case msg == "nodocker":
		where := "this machine"
		if src.Remote() {
			where = src.Host
		}
		return &DockerError{Msg: "docker CLI not found on " + where}
	case strings.HasPrefix(msg, "inspect:"):
		detail := strings.TrimSpace(strings.TrimPrefix(msg, "inspect:"))
		if strings.Contains(detail, "No such") {
			return &ContainerNotFoundError{Name: src.Container}
		}
		return &DockerError{Msg: detail}
	}
	return &DockerError{Msg: msg}
}

func splitTimestamp(line string) (time.Time, string, bool) {
	sp := strings.IndexByte(line, ' ')
	if sp <= 0 {
		return time.Time{}, "", false
	}
	t, err := time.Parse(time.RFC3339Nano, line[:sp])
	if err != nil {
		return time.Time{}, "", false
	}
	return t, line[:sp], true
}

// ReadDocker reads container logs through the docker CLI, locally or over ssh.
func ReadDocker(src config.Source, cursor string, maxLines int) (Chunk, error) {
	fp := dockerFingerprint(src)

	var prev dockerCursor
	hasCursor := decodeCursor(cursor, &prev) == nil && prev.Source == fp

	var sinceTime time.Time
	since := ""
	if hasCursor && prev.TS != "" {
		if t, err := time.Parse(time.RFC3339Nano, prev.TS); err == nil {
			sinceTime = t.Add(-resumeOverlap).UTC()
			since = sinceTime.Format(time.RFC3339Nano)
		}
	}
	expectID := ""
	if hasCursor {
		expectID = prev.ID
	}

	ctx, cancel := context.WithTimeout(context.Background(), sshTimeout)
	defer cancel()

	out, err := runScript(ctx, src, dockerScript(src.Container, since, expectID, false))
	if bytes.HasPrefix(out, []byte(errPrefix)) {
		return Chunk{}, parseDockerFailure(src, out)
	}
	if err != nil {
		return Chunk{}, err
	}

	nl := bytes.IndexByte(out, '\n')
	if nl < 0 || !bytes.HasPrefix(out, []byte(dockerIDPrefix)) {
		return Chunk{}, &InvalidSSHResponseError{Msg: "invalid docker log response header"}
	}
	id := strings.TrimSpace(string(out[len(dockerIDPrefix):nl]))
	body := out[nl+1:]

	capped := len(body) >= maxSSHBytes
	if capped {
		if last := bytes.LastIndexByte(body, '\n'); last >= 0 {
			body = body[:last+1]
		}
	}

	// A different container incarnation invalidates the old position.
	var recent []string
	var maxTS time.Time
	maxTSStr := ""
	if hasCursor && prev.ID == id {
		for i := 0; i+hashLen <= len(prev.Recent); i += hashLen {
			recent = append(recent, prev.Recent[i:i+hashLen])
		}
		maxTSStr = prev.TS
		maxTS, _ = time.Parse(time.RFC3339Nano, prev.TS)
	} else {
		sinceTime = time.Time{}
	}
	delivered := make(map[string]struct{}, len(recent))
	for _, h := range recent {
		delivered[h] = struct{}{}
	}

	raw := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
	if len(body) == 0 {
		raw = nil
	}

	lines := make([]string, 0, maxLines)
	consumedAll := true
	for _, l := range raw {
		t, tsS, ok := splitTimestamp(l)
		if ok && !sinceTime.IsZero() && t.Before(sinceTime) {
			continue
		}
		h := lineHash(l)
		if _, seen := delivered[h]; seen {
			continue
		}
		if len(lines) >= maxLines {
			consumedAll = false
			break
		}
		lines = append(lines, l)
		recent = append(recent, h)
		if ok && (maxTSStr == "" || t.After(maxTS)) {
			maxTS, maxTSStr = t, tsS
		}
	}
	if len(recent) > recentMax {
		recent = recent[len(recent)-recentMax:]
	}

	return Chunk{
		Lines:  lines,
		Cursor: encodeCursor(dockerCursor{Source: fp, ID: id, TS: maxTSStr, Recent: strings.Join(recent, "")}),
		EOF:    consumedAll && !capped,
	}, nil
}
