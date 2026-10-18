package logs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

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

func TestReadSSHRangeMatchesReadSSH(t *testing.T) {
	var scripts []string
	withSSH(t, func(args []string) ([]byte, error) {
		scripts = append(scripts, args[len(args)-1])
		return []byte("__MCPLOGVIEW_SIZE__:12\nl1\nl2\nl3\n"), nil
	})
	src := sshSource()
	chunk, err := ReadSSH(src, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	r, err := ReadSSHRange(context.Background(), src, src.Path, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if scripts[0] != scripts[1] {
		t.Fatalf("remote command differs:\n%s\n%s", scripts[0], scripts[1])
	}
	if strings.Join(r.Lines, ",") != strings.Join(chunk.Lines, ",") || r.EOF != chunk.EOF {
		t.Fatalf("range %+v vs chunk %+v", r, chunk)
	}
	if r.Offset != 6 || r.Size != 12 {
		t.Fatalf("offset/size: %+v", r)
	}

	// offset is passed through and an offset past the end restarts at 0
	if _, err := ReadSSHRange(context.Background(), src, "/other/p", 6, 2); err != nil || !strings.Contains(scripts[2], "REQ_OFFSET=6") || !strings.Contains(scripts[2], "/other/p") {
		t.Fatalf("offset/path not passed: %v %s", err, scripts[2])
	}
	r, err = ReadSSHRange(context.Background(), src, src.Path, 99, 10)
	if err != nil {
		t.Fatal(err)
	}
	if r.Offset != 9 || strings.Join(r.Lines, ",") != "l1,l2,l3" || !r.EOF {
		t.Fatalf("past-end offset must restart at 0: %+v", r)
	}
}

func TestRunScriptUsesSSHForRemoteSources(t *testing.T) {
	var got []string
	withSSH(t, func(args []string) ([]byte, error) { got = args; return []byte("out"), nil })
	out, err := RunScript(context.Background(), sshSource(), "echo 'x'")
	if err != nil || string(out) != "out" {
		t.Fatalf("%q %v", out, err)
	}
	if got[len(got)-1] != "sh -lc "+shellQuote("echo 'x'") || !strings.Contains(strings.Join(got, " "), "-i /k") {
		t.Fatalf("args: %q", got)
	}
}

// withStream replaces the bounded runner; fn gets the command name and args.
func withStream(t *testing.T, fn func(ctx context.Context, name string, args []string, stdout io.Writer) error) {
	t.Helper()
	old := streamRun
	streamRun = fn
	t.Cleanup(func() { streamRun = old })
}

func TestRunScriptLimitStopsAtLimit(t *testing.T) {
	const limit = 1000
	var ctxErr error
	writes := 0
	withStream(t, func(ctx context.Context, name string, args []string, stdout io.Writer) error {
		chunk := bytes.Repeat([]byte("x"), 300)
		for ctx.Err() == nil && writes < 1_000_000 { // an endless remote "cat"
			writes++
			if _, err := stdout.Write(chunk); err != nil {
				break
			}
		}
		ctxErr = ctx.Err()
		return errors.New("signal: killed")
	})
	out, err := RunScriptLimit(context.Background(), sshSource(), "cat /huge", limit)
	if !errors.Is(err, ErrOutputTooLarge) || out != nil {
		t.Fatalf("got %d bytes, %v", len(out), err)
	}
	if ctxErr == nil {
		t.Fatal("the command's context must be cancelled once the limit is exceeded")
	}
	if writes > 5 {
		t.Fatalf("runner kept writing after the limit: %d writes", writes)
	}
}

func TestCapWriterRetainsAtMostLimit(t *testing.T) {
	cancelled := false
	w := &capWriter{limit: 10, cancel: func() { cancelled = true }}
	for i := 0; i < 5; i++ {
		_, _ = w.Write([]byte("abcdefgh"))
	}
	if w.buf.Len() > 10 || !w.over || !cancelled {
		t.Fatalf("len %d over %v cancelled %v", w.buf.Len(), w.over, cancelled)
	}
}

func TestRunScriptLimitExactlyAtLimitSucceeds(t *testing.T) {
	var gotName string
	var gotArgs []string
	withStream(t, func(_ context.Context, name string, args []string, stdout io.Writer) error {
		gotName, gotArgs = name, args
		_, err := stdout.Write(bytes.Repeat([]byte("y"), 1000))
		return err
	})
	out, err := RunScriptLimit(context.Background(), sshSource(), "echo 'x'", 1000)
	if err != nil || len(out) != 1000 {
		t.Fatalf("%d %v", len(out), err)
	}
	if gotName != "ssh" || gotArgs[len(gotArgs)-1] != "sh -lc "+shellQuote("echo 'x'") {
		t.Fatalf("ran %s %q", gotName, gotArgs)
	}
}

func TestRunScriptLimitPassesCommandErrors(t *testing.T) {
	withStream(t, func(_ context.Context, _ string, _ []string, stdout io.Writer) error {
		_, _ = stdout.Write([]byte("partial"))
		return errors.New("exit status 2: boom")
	})
	if _, err := RunScriptLimit(context.Background(), sshSource(), "false", 1000); err == nil || errors.Is(err, ErrOutputTooLarge) || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("got %v", err)
	}
}

func TestRunScriptLimitKillsRealProcess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := RunScriptLimit(ctx, config.Source{}, "yes", 1<<20) // endless output
	if !errors.Is(err, ErrOutputTooLarge) || ctx.Err() != nil {
		t.Fatalf("got %v (ctx %v)", err, ctx.Err())
	}
}

func TestRunScriptLimitLargeStderrDoesNotStall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// ~1 MiB of stderr, far past maxStderrBytes, then a real exit status.
	script := `i=0; while [ $i -lt 16384 ]; do echo 'permission denied: 0123456789012345678901234567890123456789012345' >&2; i=$((i+1)); done; echo out; exit 3`
	start := time.Now()
	out, err := RunScriptLimit(ctx, config.Source{}, script, 1<<20)
	if ctx.Err() != nil || time.Since(start) > 4*time.Second {
		t.Fatalf("stalled: %v after %s", ctx.Err(), time.Since(start))
	}
	if err == nil || !strings.Contains(err.Error(), "exit status 3") || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("exit status must survive: %.200v", err)
	}
	if len(err.Error()) > maxStderrBytes+100 {
		t.Fatalf("retained stderr not capped: %d bytes", len(err.Error()))
	}
	if out != nil {
		t.Fatalf("out on error: %q", out)
	}
}

func TestDiscardExcessNeverFails(t *testing.T) {
	w := &discardExcess{limit: 10}
	for i := 0; i < 5; i++ {
		if n, err := w.Write([]byte("abcdefgh")); n != 8 || err != nil {
			t.Fatalf("write %d: %d %v", i, n, err)
		}
	}
	if w.buf.Len() != 10 {
		t.Fatalf("kept %d", w.buf.Len())
	}
}

func TestBuildSSHArgsNoMultiplexByDefault(t *testing.T) {
	got := strings.Join(BuildSSHArgs(sshSource(), "cmd"), " ")
	if strings.Contains(got, "Control") || strings.Contains(got, "ConnectTimeout") {
		t.Fatalf("plain source must not multiplex: %q", got)
	}
}

func TestBuildSSHArgsMultiplex(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("never multiplexes on windows")
	}
	s := sshSource()
	s.Multiplex = true
	args := BuildSSHArgs(s, "cmd")
	got := strings.Join(args, " ")
	dir := defaultMuxDir()
	for _, w := range []string{"-o ConnectTimeout=10", "-o ControlMaster=auto", "-o ControlPersist=60", "-o ControlPath=" + dir + "/%C"} {
		if !strings.Contains(got, w) {
			t.Fatalf("missing %q in %q", w, got)
		}
	}
	if !strings.HasSuffix(got, "u@h cmd") {
		t.Fatalf("target and command must stay last: %q", got)
	}
	if len(dir)+1+40 > 104 {
		t.Fatalf("control path too long for a unix socket: %d", len(dir)+1+40)
	}
	fi, err := os.Lstat(dir)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir %s: %v %v", dir, fi, err)
	}
}

func TestPrepareMuxDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix only")
	}
	base := t.TempDir()

	fresh := filepath.Join(base, "fresh")
	if got := prepareMuxDir(fresh); got != fresh {
		t.Fatalf("fresh: %q", got)
	}
	if fi, _ := os.Stat(fresh); fi.Mode().Perm() != 0o700 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if got := prepareMuxDir(fresh); got != fresh {
		t.Fatalf("existing good dir: %q", got)
	}

	open := filepath.Join(base, "open")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := prepareMuxDir(open); got != "" {
		t.Fatalf("wrong mode must fall back: %q", got)
	}

	link := filepath.Join(base, "link")
	if err := os.Symlink(fresh, link); err != nil {
		t.Fatal(err)
	}
	if got := prepareMuxDir(link); got != "" {
		t.Fatalf("symlink must fall back: %q", got)
	}

	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := prepareMuxDir(file); got != "" {
		t.Fatalf("regular file must fall back: %q", got)
	}
}

func withMuxDir(t *testing.T, dir string) {
	t.Helper()
	old := muxDirFunc
	muxDirFunc = func() string { return dir }
	t.Cleanup(func() { muxDirFunc = old })
}

// Multiplexed (plugin) sources always get a connect timeout and keepalives,
// even when the control-socket directory is unusable (or on Windows).
func TestBuildSSHArgsMultiplexWithoutMuxDir(t *testing.T) {
	withMuxDir(t, "")
	s := sshSource()
	s.Multiplex = true
	got := strings.Join(BuildSSHArgs(s, "cmd"), " ")
	for _, w := range []string{"-o ConnectTimeout=10", "-o ServerAliveInterval=5", "-o ServerAliveCountMax=2"} {
		if !strings.Contains(got, w) {
			t.Fatalf("missing %q in %q", w, got)
		}
	}
	if strings.Contains(got, "Control") {
		t.Fatalf("no usable mux dir must not add Control options: %q", got)
	}
	if !strings.HasSuffix(got, "u@h cmd") {
		t.Fatalf("target and command must stay last: %q", got)
	}
}

func TestBuildSSHArgsMultiplexHasServerAlive(t *testing.T) {
	withMuxDir(t, "/tmp/mcplv-test")
	s := sshSource()
	s.Multiplex = true
	got := strings.Join(BuildSSHArgs(s, "cmd"), " ")
	for _, w := range []string{"-o ConnectTimeout=10", "-o ServerAliveInterval=5", "-o ServerAliveCountMax=2", "-o ControlPath=/tmp/mcplv-test/%C"} {
		if !strings.Contains(got, w) {
			t.Fatalf("missing %q in %q", w, got)
		}
	}
	plain := strings.Join(BuildSSHArgs(sshSource(), "cmd"), " ")
	if strings.Contains(plain, "ServerAlive") {
		t.Fatalf("plain source must not get keepalives: %q", plain)
	}
}
