package logs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skromesch/mcplogview-go/internal/config"
)

func tmpLog(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "a.log")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadLocalPaging(t *testing.T) {
	p := tmpLog(t, "a\nb\nc\nd\n")

	r1, err := ReadLocal(p, "", 2)
	if err != nil || strings.Join(r1.Lines, ",") != "a,b" || r1.EOF {
		t.Fatalf("page1: %+v %v", r1, err)
	}
	r2, _ := ReadLocal(p, r1.Cursor, 2)
	if strings.Join(r2.Lines, ",") != "c,d" || !r2.EOF {
		t.Fatalf("page2 (exact fit must report eof): %+v", r2)
	}
	r3, _ := ReadLocal(p, r2.Cursor, 2)
	if len(r3.Lines) != 0 || !r3.EOF || r3.Lines == nil {
		t.Fatalf("page3 must be an empty non-nil slice: %+v", r3)
	}

	// appended data is picked up from the old cursor
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString("e\n")
	_ = f.Close()
	r4, _ := ReadLocal(p, r3.Cursor, 10)
	if strings.Join(r4.Lines, ",") != "e" {
		t.Fatalf("append: %+v", r4)
	}
}

func TestReadLocalPartialLastLineAndMultibyte(t *testing.T) {
	p := tmpLog(t, "héllo\nwörld")
	r, _ := ReadLocal(p, "", 10)
	if strings.Join(r.Lines, "|") != "héllo|wörld" || !r.EOF {
		t.Fatalf("%+v", r)
	}
	// offsets are byte based: continuing must yield nothing, not garbage
	r2, _ := ReadLocal(p, r.Cursor, 10)
	if len(r2.Lines) != 0 {
		t.Fatalf("%+v", r2)
	}
}

func TestReadLocalTruncationAndBadCursor(t *testing.T) {
	p := tmpLog(t, "one\ntwo\nthree\n")
	r, _ := ReadLocal(p, "", 10)

	if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil { // truncate in place
		t.Fatal(err)
	}
	r2, _ := ReadLocal(p, r.Cursor, 10)
	if strings.Join(r2.Lines, ",") != "x" {
		t.Fatalf("truncated file must restart from 0: %+v", r2)
	}

	r3, err := ReadLocal(p, "%%%not-base64%%%", 10)
	if err != nil || strings.Join(r3.Lines, ",") != "x" {
		t.Fatalf("bad cursor must restart: %+v %v", r3, err)
	}
}

func TestReadLocalMissingFile(t *testing.T) {
	if _, err := ReadLocal(filepath.Join(t.TempDir(), "nope"), "", 1); err == nil {
		t.Fatal("expected error")
	}
}

func TestReadLocalLargeFileDoesNotNeedWholeRead(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 200000; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	p := tmpLog(t, b.String())
	r, _ := ReadLocal(p, "", 3)
	if strings.Join(r.Lines, ",") != "line 0,line 1,line 2" || r.EOF {
		t.Fatalf("%+v", r)
	}
}

func sshSource() config.Source {
	return config.Source{Type: config.SourceSSH, ID: "r", Label: "R", Host: "h", Port: 22, User: "u", IdentityFile: "/k", Path: "/var/log/it's.log"}
}

func withSSH(t *testing.T, fn func(args []string) ([]byte, error)) {
	t.Helper()
	old := sshRun
	sshRun = func(_ context.Context, args []string) ([]byte, error) { return fn(args) }
	t.Cleanup(func() { sshRun = old })
}

func TestBuildSSHArgs(t *testing.T) {
	s := sshSource()
	s.StrictHostKeyChecking = true
	got := strings.Join(BuildSSHArgs(s, "cmd"), " ")
	want := "-i /k -p 22 -o BatchMode=yes -o StrictHostKeyChecking=yes u@h cmd"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestReadSSHHappyPathAndCursor(t *testing.T) {
	var gotArgs []string
	withSSH(t, func(args []string) ([]byte, error) {
		gotArgs = args
		return []byte("__MCPLOGVIEW_SIZE__:12\nl1\nl2\nl3\n"), nil
	})
	r, err := ReadSSH(sshSource(), "", 2)
	if err != nil || strings.Join(r.Lines, ",") != "l1,l2" || r.EOF {
		t.Fatalf("%+v %v", r, err)
	}
	remote := gotArgs[len(gotArgs)-1]
	if !strings.Contains(remote, `'\''`) || !strings.Contains(remote, "REQ_OFFSET=0") || !strings.Contains(remote, "head -c") {
		t.Fatalf("remote command: %s", remote)
	}

	// the returned cursor drives the next request offset
	r2, _ := ReadSSH(sshSource(), r.Cursor, 2)
	_ = r2
	if !strings.Contains(gotArgs[len(gotArgs)-1], "REQ_OFFSET=6") {
		t.Fatalf("second request must continue at byte 6: %s", gotArgs[len(gotArgs)-1])
	}

	// cursor from another source is ignored
	other := sshSource()
	other.Host = "elsewhere"
	_, _ = ReadSSH(other, r.Cursor, 2)
	if !strings.Contains(gotArgs[len(gotArgs)-1], "REQ_OFFSET=0") {
		t.Fatal("foreign cursor must be ignored")
	}
}

func TestReadSSHErrors(t *testing.T) {
	t.Run("missing file via exit status", func(t *testing.T) {
		withSSH(t, func([]string) ([]byte, error) {
			return []byte("__MCPLOGVIEW_ERR__:missing\n"), errors.New("exit status 12")
		})
		var nf *RemoteFileNotFoundError
		if _, err := ReadSSH(sshSource(), "", 1); !errors.As(err, &nf) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("transport failure surfaces", func(t *testing.T) {
		withSSH(t, func([]string) ([]byte, error) { return nil, errors.New("permission denied") })
		if _, err := ReadSSH(sshSource(), "", 1); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("bad header", func(t *testing.T) {
		withSSH(t, func([]string) ([]byte, error) { return []byte("garbage\nx\n"), nil })
		var inv *InvalidSSHResponseError
		if _, err := ReadSSH(sshSource(), "", 1); !errors.As(err, &inv) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("no newline at all", func(t *testing.T) {
		withSSH(t, func([]string) ([]byte, error) { return []byte("x"), nil })
		var inv *InvalidSSHResponseError
		if _, err := ReadSSH(sshSource(), "", 1); !errors.As(err, &inv) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestReadSSHCapKeepsWholeLinesOnly(t *testing.T) {
	line := strings.Repeat("x", 99) + "\n"
	body := strings.Repeat(line, maxSSHBytes/len(line)+1)
	body = body[:maxSSHBytes-7] // cap hit in the middle of a line
	// make len(body) hit exactly the cap, as `head -c` would
	out := "__MCPLOGVIEW_SIZE__:999999999\n" + body
	out += strings.Repeat("y", maxSSHBytes-len(body))
	withSSH(t, func([]string) ([]byte, error) { return []byte(out), nil })

	r, err := ReadSSH(sshSource(), "", 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if r.EOF {
		t.Fatal("capped read must not claim eof")
	}
	for _, l := range r.Lines {
		if strings.Contains(l, "y") {
			t.Fatal("partial trailing line leaked into the result")
		}
	}
}
