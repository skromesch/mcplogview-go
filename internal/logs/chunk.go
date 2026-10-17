// Package logs reads chunks of lines from local and SSH log sources.
package logs

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/skromesch/mcplogview-go/internal/config"
)

// Chunk is the result of a read: the lines, the cursor to continue from and
// whether the end of the file was reached.
type Chunk struct {
	Lines  []string `json:"lines"`
	Cursor string   `json:"cursor"`
	EOF    bool     `json:"eof"`
}

// RemoteFileNotFoundError is returned when the remote log file is missing.
type RemoteFileNotFoundError struct{ Path string }

func (e *RemoteFileNotFoundError) Error() string {
	return fmt.Sprintf("remote log file not found: %s", e.Path)
}

// InvalidSSHResponseError is returned for malformed remote responses.
type InvalidSSHResponseError struct{ Msg string }

func (e *InvalidSSHResponseError) Error() string { return e.Msg }

// ReadChunk dispatches on the source type.
func ReadChunk(src config.Source, cursor string, maxLines int) (Chunk, error) {
	switch src.Type {
	case config.SourceSSH:
		return ReadSSH(src, cursor, maxLines)
	case config.SourceDocker:
		return ReadDocker(src, cursor, maxLines)
	}
	return ReadLocal(src.Path, cursor, maxLines)
}

func encodeCursor(v any) string {
	b, _ := json.Marshal(v)
	return base64.StdEncoding.EncodeToString(b)
}

func decodeCursor(cursor string, out any) error {
	if cursor == "" {
		return errors.New("empty cursor")
	}
	b, err := base64.StdEncoding.DecodeString(cursor)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// extractLines splits up to maxLines lines out of chunk. A trailing line
// without newline is returned as well (and counts as consumed).
func extractLines(chunk []byte, maxLines int) (lines []string, consumed int, eof bool) {
	lines = make([]string, 0, maxLines)
	index := 0
	for len(lines) < maxLines {
		nl := bytes.IndexByte(chunk[index:], '\n')
		if nl < 0 {
			if index < len(chunk) {
				lines = append(lines, string(chunk[index:]))
				consumed = len(chunk)
			}
			break
		}
		lines = append(lines, string(chunk[index:index+nl]))
		index += nl + 1
		consumed = index
	}
	return lines, consumed, consumed >= len(chunk)
}
