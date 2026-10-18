package plugin

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/skromesch/mcplogview-go/internal/config"
	"github.com/skromesch/mcplogview-go/internal/logs"
)

// fakeSSH records what the capability hands to the backend.
type fakeSSH struct {
	mu       sync.Mutex
	src      config.Source
	script   string
	path     string
	offset   int64
	maxLines int
	calls    int
	ctxErr   bool // the call context carried a deadline

	out    []byte
	result logs.RangeResult
	err    error
}

func (f *fakeSSH) Exec(ctx context.Context, src config.Source, script string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.src, f.script = src, script
	_, f.ctxErr = ctx.Deadline()
	return f.out, f.err
}

func (f *fakeSSH) Tail(ctx context.Context, src config.Source, path string, offset int64, maxLines int) (logs.RangeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.src, f.path, f.offset, f.maxLines = src, path, offset, maxLines
	_, f.ctxErr = ctx.Deadline()
	return f.result, f.err
}

// sshPlugin registers tool "x" running body with args a.
func sshPlugin(body string) string {
	return toolPlugin(`mcp.tool.register{ name = "x", handler = function(a) ` + body + ` end }`)
}

func sshRef() config.PluginRef {
	return config.PluginRef{
		Name:  "p",
		Allow: []string{config.CapSSH},
		SSH: &config.PluginSSH{
			AllowedCIDRs: []string{"192.0.2.0/24", "192.168.1.0/28"},
			User:         "stations", IdentityFile: "/keys/stations", Port: 2222, StrictHostKeyChecking: true,
		},
	}
}

func sshTool(t *testing.T, f *fakeSSH, ref config.PluginRef, body string) fakeTool {
	t.Helper()
	dir := t.TempDir()
	writePlugin(t, dir, "p", sshPlugin(body))
	h, s := hostWithSink(t, Options{SSH: f}, plugins(dir, ref))
	if st := statusOf(t, h, "p"); !st.OK {
		t.Fatalf("load: %+v", st)
	}
	return s.get(t, "x")
}

func callX(t *testing.T, tl fakeTool, args map[string]any) (map[string]any, error) {
	t.Helper()
	res, err := tl.call(context.Background(), args)
	if err != nil {
		return nil, err
	}
	m, _ := res.(map[string]any)
	return m, nil
}

func TestSSHNotAvailableWithoutAllow(t *testing.T) {
	ref := sshRef()
	ref.Allow = nil
	tl := sshTool(t, &fakeSSH{}, ref, `return { missing = mcp.ssh == nil }`)
	m, err := callX(t, tl, nil)
	if err != nil || m["missing"] != true {
		t.Fatalf("%v %v", m, err)
	}
}

func TestSSHRejectsHostsOutsideCIDR(t *testing.T) {
	f := &fakeSSH{}
	tl := sshTool(t, f, sshRef(), `return { out = mcp.ssh(a.host):exec("true") }`)
	for _, host := range []string{
		"192.0.3.5", "192.0.2.256", "example.com", "192.0.2.4; id", "",
		"::1", "192.0.2.4%eth0", " 192.0.2.4", "0192.0.2.4", "::ffff:192.0.2.4",
		"192.168.1.16", "192.0.2.4\n", "192.0.2",
	} {
		_, err := callX(t, tl, map[string]any{"host": host})
		if err == nil || !strings.Contains(err.Error(), "not allowed") || !strings.Contains(err.Error(), fmt.Sprintf("%q", host)) {
			t.Errorf("host %q: %v", host, err)
		}
	}
	if f.calls != 0 {
		t.Fatalf("backend called %d times for rejected hosts", f.calls)
	}
	for _, host := range []string{"192.0.2.4", "192.0.2.255", "192.168.1.15"} {
		if _, err := callX(t, tl, map[string]any{"host": host}); err != nil {
			t.Errorf("host %q rejected: %v", host, err)
		}
	}
}

func TestSSHHostMustBeString(t *testing.T) {
	tl := sshTool(t, &fakeSSH{}, sshRef(), `return { out = mcp.ssh({}):exec("true") }`)
	if _, err := callX(t, tl, nil); err == nil {
		t.Fatal("table host accepted")
	}
}

func TestSSHExecReturnsStdoutAndPassesConfiguredIdentity(t *testing.T) {
	f := &fakeSSH{out: []byte("2026-10-18\n")}
	tl := sshTool(t, f, sshRef(), `return { out = mcp.ssh("192.0.2.45"):exec("date +%F") }`)
	m, err := callX(t, tl, nil)
	if err != nil || m["out"] != "2026-10-18\n" {
		t.Fatalf("%v %v", m, err)
	}
	want := config.Source{
		Type: config.SourceSSH, Host: "192.0.2.45", User: "stations", IdentityFile: "/keys/stations",
		Port: 2222, StrictHostKeyChecking: true, Multiplex: true,
	}
	got := f.src
	if got.Type != want.Type || got.Host != want.Host || got.User != want.User || got.IdentityFile != want.IdentityFile ||
		got.Port != want.Port || got.StrictHostKeyChecking != want.StrictHostKeyChecking || got.Multiplex != want.Multiplex {
		t.Fatalf("source: %+v", got)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("source must be valid: %v", err)
	}
	if f.script != "date +%F" {
		t.Fatalf("script: %q", f.script)
	}
	if !f.ctxErr {
		t.Fatal("backend must get the plugin call context (with its timeout)")
	}
}

func TestSSHExecErrorMentionsHost(t *testing.T) {
	f := &fakeSSH{err: errors.New("exit status 1: grep: bad")}
	tl := sshTool(t, f, sshRef(), `return { out = mcp.ssh("192.0.2.45"):exec("grep") }`)
	_, err := callX(t, tl, nil)
	if err == nil || !strings.Contains(err.Error(), "192.0.2.45: exit status 1: grep: bad") {
		t.Fatalf("err: %v", err)
	}

	// exit status 255 is ssh's own failure: connection problems
	ee := exec.Command("sh", "-c", "exit 255").Run()
	f.err = fmt.Errorf("%w: %s", ee, "ssh: connect to host 192.0.2.45 port 2222: Connection refused")
	_, err = callX(t, tl, nil)
	if err == nil || !strings.Contains(err.Error(), "cannot reach 192.0.2.45: ssh: connect to host 192.0.2.45 port 2222: Connection refused") {
		t.Fatalf("err: %v", err)
	}
}

func TestSSHExecLimits(t *testing.T) {
	f := &fakeSSH{}
	tl := sshTool(t, f, sshRef(), `return { out = mcp.ssh("192.0.2.45"):exec(a.cmd) }`)
	if _, err := callX(t, tl, map[string]any{"cmd": strings.Repeat("x", maxSSHCommand)}); err != nil {
		t.Fatalf("command at the limit rejected: %v", err)
	}
	f.calls = 0
	for _, cmd := range []string{strings.Repeat("x", maxSSHCommand+1), "echo a\x00b"} {
		if _, err := callX(t, tl, map[string]any{"cmd": cmd}); err == nil {
			t.Errorf("command of %d bytes accepted", len(cmd))
		}
	}
	if f.calls != 0 {
		t.Fatal("backend called for a rejected command")
	}

	f.err = logs.ErrOutputTooLarge
	if _, err := callX(t, tl, map[string]any{"cmd": "cat big"}); err == nil || !strings.Contains(err.Error(), "192.0.2.45: output too large (limit 4 MiB)") {
		t.Fatalf("backend overflow: %v", err)
	}
	f.err = nil
	f.out = make([]byte, maxSSHOutput+1)
	if _, err := callX(t, tl, map[string]any{"cmd": "cat big"}); err == nil || !strings.Contains(err.Error(), "output too large") {
		t.Fatalf("oversized output: %v", err)
	}
}

func TestSSHTailReturnsTable(t *testing.T) {
	f := &fakeSSH{result: logs.RangeResult{Lines: []string{"a", "b"}, Offset: 42, Size: 100, EOF: false}}
	tl := sshTool(t, f, sshRef(), `local r = mcp.ssh("192.0.2.45"):tail("/var/log/x.log", a.offset, a.max)
return { lines = r.lines, offset = r.offset, size = r.size, eof = r.eof, n = #r.lines }`)
	m, err := callX(t, tl, map[string]any{"offset": 10.0, "max": 2.0})
	if err != nil {
		t.Fatal(err)
	}
	if m["offset"] != 42.0 || m["size"] != 100.0 || m["eof"] != false || m["n"] != 2.0 {
		t.Fatalf("result: %#v", m)
	}
	if f.path != "/var/log/x.log" || f.offset != 10 || f.maxLines != 2 || f.src.Host != "192.0.2.45" || !f.ctxErr {
		t.Fatalf("backend got %+v", f)
	}

	// an empty result is still an (empty) array
	f.result = logs.RangeResult{Lines: []string{}, Offset: 0, Size: 0, EOF: true}
	if m, err = callX(t, tl, map[string]any{"offset": 0.0, "max": 5.0}); err != nil || m["n"] != 0.0 || m["eof"] != true {
		t.Fatalf("empty: %#v %v", m, err)
	}

	// max_lines is clamped to 1..500
	for _, c := range []struct{ in, want float64 }{{0, 1}, {-3, 1}, {501, 500}, {100000, 500}} {
		if _, err := callX(t, tl, map[string]any{"offset": 0.0, "max": c.in}); err != nil || f.maxLines != int(c.want) {
			t.Errorf("max %v -> %d (%v)", c.in, f.maxLines, err)
		}
	}
	// max_lines is optional
	if _, err := callX(t, tl, map[string]any{"offset": 0.0}); err != nil || f.maxLines != defaultTailLines {
		t.Errorf("default max -> %d (%v)", f.maxLines, err)
	}
}

func TestSSHTailRejectsRelativeAndNewlinePaths(t *testing.T) {
	f := &fakeSSH{result: logs.RangeResult{Lines: []string{}}}
	tl := sshTool(t, f, sshRef(), `local r = mcp.ssh("192.0.2.45"):tail(a.path, a.offset, 10) return { n = #r.lines }`)
	for _, path := range []string{"var/log/x", "", "./x", "/var/log/x\n; id", "/var/log/x\x00", "/var/log/x\r"} {
		if _, err := callX(t, tl, map[string]any{"path": path, "offset": 0.0}); err == nil {
			t.Errorf("path %q accepted", path)
		}
	}
	for _, off := range []float64{-1, 1.5} {
		if _, err := callX(t, tl, map[string]any{"path": "/var/log/x", "offset": off}); err == nil {
			t.Errorf("offset %v accepted", off)
		}
	}
	if f.calls != 0 {
		t.Fatal("backend called for a rejected tail")
	}
}

func TestSSHTailErrorMentionsHost(t *testing.T) {
	f := &fakeSSH{err: &logs.RemoteFileNotFoundError{Path: "/var/log/x"}}
	tl := sshTool(t, f, sshRef(), `return mcp.ssh("192.0.2.45"):tail("/var/log/x", 0, 10)`)
	_, err := callX(t, tl, nil)
	if err == nil || !strings.Contains(err.Error(), "192.0.2.45: remote log file not found: /var/log/x") {
		t.Fatalf("err: %v", err)
	}
}

func TestSSHDefaultPortIs22(t *testing.T) {
	f := &fakeSSH{}
	ref := sshRef()
	ref.SSH.Port = 0
	tl := sshTool(t, f, ref, `return { out = mcp.ssh("192.0.2.45"):exec("true") }`)
	if _, err := callX(t, tl, nil); err != nil || f.src.Port != 22 || f.src.Validate() != nil {
		t.Fatalf("%+v %v", f.src, err)
	}
}

func TestSSHRejectsIPv6EvenWhenCIDRAllowsIt(t *testing.T) {
	f := &fakeSSH{}
	ref := sshRef()
	ref.SSH.AllowedCIDRs = []string{"fd00::/8", "::/0"}
	tl := sshTool(t, f, ref, `return { out = mcp.ssh(a.host):exec("true") }`)
	for _, host := range []string{"fd00::1", "::1", "::ffff:192.0.2.4"} {
		if _, err := callX(t, tl, map[string]any{"host": host}); err == nil || !strings.Contains(err.Error(), "not allowed") {
			t.Errorf("host %q: %v", host, err)
		}
	}
	if f.calls != 0 {
		t.Fatal("backend called for an IPv6 host")
	}
}
