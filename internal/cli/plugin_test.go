package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestPluginSyncListRemove(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	work := sandbox(t)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")

	src := filepath.Join(t.TempDir(), "src")
	bare := filepath.Join(t.TempDir(), "demo.git")
	gitT(t, work, "init", "-q", "-b", "main", src)
	_ = os.WriteFile(filepath.Join(src, "init.lua"), []byte("return { api_version = 1 }\n"), 0o644)
	gitT(t, src, "add", ".")
	gitT(t, src, "commit", "-q", "-m", "c")
	gitT(t, work, "clone", "-q", "--bare", "--", src, bare)
	head := gitT(t, src, "rev-parse", "HEAD")

	cfg := `{"plugins": [{"name": "demo", "git": "` + bare + `"}, {"name": "local"}]}`
	if err := os.WriteFile(filepath.Join(work, "mcplogview.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := run(t, "plugin", "list")
	if code != 0 || !strings.Contains(out, "missing") {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	code, out, errOut = run(t, "plugin", "sync")
	if code != 0 || !strings.Contains(out, "demo  cloned  "+head[:7]) {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(work, "plugins.lock.json")); err != nil {
		t.Fatal(err)
	}
	code, out, errOut = run(t, "plugin", "list")
	if code != 0 || !strings.Contains(out, "demo") || !strings.Contains(out, " ok ") || !strings.Contains(out, "load OK") {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	// the non-git plugin is missing on disk: its load error shows
	if !strings.Contains(out, "local") || !strings.Contains(out, "load error") {
		t.Fatalf("%q", out)
	}
	code, out, errOut = run(t, "plugin", "remove", "demo")
	if code != 0 || !strings.Contains(out, "removed") {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(work, "plugins", "demo")); !os.IsNotExist(err) {
		t.Fatal("not removed")
	}
}

func TestPluginUsageErrors(t *testing.T) {
	sandbox(t)
	if code, _, errOut := run(t, "plugin"); code != 2 || !strings.Contains(errOut, "sync") {
		t.Fatalf("%d %q", code, errOut)
	}
	if code, _, _ := run(t, "plugin", "frob"); code != 2 {
		t.Fatal(code)
	}
	if code, _, _ := run(t, "plugin", "remove"); code != 2 {
		t.Fatal(code)
	}
	if code, _, errOut := run(t, "plugin", "sync", "ghost"); code != 1 || !strings.Contains(errOut, "not in the config") {
		t.Fatalf("%d %q", code, errOut)
	}
}

func TestPluginTest(t *testing.T) {
	sandbox(t)
	dir := filepath.Join(t.TempDir(), "demo")
	_ = os.MkdirAll(filepath.Join(dir, "tests"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "init.lua"), []byte(`mcp.tool.register{name="demo_hi", description="hi", handler=function() return "hi" end}
return { api_version = 1 }`), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "tests", "demo_test.lua"), []byte(`
test.case("good", function() local h = test.load{}; test.ok(h:status()); test.eq(h:tool("demo_hi"), "hi") end)
test.case("bad", function() test.eq(1, 2) end)
`), 0o644)

	code, out, errOut := run(t, "plugin", "test", dir)
	if code != 1 || !strings.Contains(out, "ok   demo_test.lua: good") || !strings.Contains(out, "FAIL demo_test.lua: bad") || !strings.Contains(out, "1 passed, 1 failed") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
	code, out, errOut = run(t, "plugin", "test", "-run", "^good$", dir)
	if code != 0 || strings.Contains(out, "bad") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
	code, out, _ = run(t, "plugin", "test", dir, "-run", "bad")
	if code != 1 || strings.Contains(out, "good") {
		t.Fatalf("flag after dir: code=%d out=%q", code, out)
	}
	if code, _, _ := run(t, "plugin", "test"); code != 2 {
		t.Fatalf("missing dir: code=%d", code)
	}
	if code, _, _ := run(t, "plugin", "test", "-run", "(", dir); code != 2 {
		t.Fatalf("bad pattern: code=%d", code)
	}
	if code, _, errOut := run(t, "plugin", "test", t.TempDir()); code != 1 || !strings.Contains(errOut, "no tests") {
		t.Fatalf("no tests: code=%d err=%q", code, errOut)
	}
}
