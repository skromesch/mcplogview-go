package logs

import (
	"bufio"
	"io"
	"os"
	"strings"
)

type localCursor struct {
	Inode  uint64 `json:"inode"`
	Offset int64  `json:"offset"`
}

// ReadLocal reads up to maxLines lines from path starting at the cursor. It
// seeks to the stored offset instead of loading the whole file. The cursor is
// ignored (restart from 0) when the inode changed or the file got truncated.
func ReadLocal(path, cursor string, maxLines int) (Chunk, error) {
	f, err := os.Open(path)
	if err != nil {
		return Chunk{}, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return Chunk{}, err
	}
	ino := inodeOf(st)

	var offset int64
	var prev localCursor
	if decodeCursor(cursor, &prev) == nil && prev.Inode == ino && prev.Offset >= 0 && prev.Offset <= st.Size() {
		offset = prev.Offset
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return Chunk{}, err
	}

	r := bufio.NewReaderSize(f, 64*1024)
	lines := make([]string, 0, maxLines)
	var consumed int64
	eof := false

	for len(lines) < maxLines {
		s, rerr := r.ReadString('\n')
		if len(s) > 0 {
			consumed += int64(len(s))
			lines = append(lines, strings.TrimSuffix(s, "\n"))
		}
		if rerr != nil {
			if rerr != io.EOF {
				return Chunk{}, rerr
			}
			eof = true
			break
		}
	}
	if !eof {
		_, perr := r.Peek(1)
		eof = perr == io.EOF
	}

	return Chunk{
		Lines:  lines,
		Cursor: encodeCursor(localCursor{Inode: ino, Offset: offset + consumed}),
		EOF:    eof,
	}, nil
}
