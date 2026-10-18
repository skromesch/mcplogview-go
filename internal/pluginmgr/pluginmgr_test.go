package pluginmgr

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skromesch/mcplogview-go/internal/config"
)

// gitEnv isolates git from the user's configuration.
func gitEnv(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitRun(context.Background(), dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(out)
}

// upstream is a bare repo plus a work tree that pushes to it.
type upstream struct {
	t          *testing.T
	bare, work string
}

func newUpstream(t *testing.T) *upstream {
	gitEnv(t)
	root := t.TempDir()
	u := &upstream{t: t, bare: filepath.Join(root, "plugin.git"), work: filepath.Join(root, "work")}
	git(t, root, "init", "--bare", "-b", "main", u.bare)
	git(t, root, "init", "-b", "main", u.work)
	git(t, u.work, "remote", "add", "origin", u.bare)
	u.commit("return { setup = function() end }\n")
	return u
}

// commit writes init.lua, commits, pushes and returns the commit.
func (u *upstream) commit(body string) string {
	if err := os.WriteFile(filepath.Join(u.work, "init.lua"), []byte(body), 0o644); err != nil {
		u.t.Fatal(err)
	}
	git(u.t, u.work, "add", "init.lua")
	git(u.t, u.work, "commit", "-q", "-m", "c")
	git(u.t, u.work, "push", "-q", "origin", "main", "--tags")
	return git(u.t, u.work, "rev-parse", "HEAD")
}

func setup(t *testing.T, u *upstream, ref string) (*Manager, *bytes.Buffer) {
	base := t.TempDir()
	cfg := config.Defaults()
	cfg.Plugins = []config.PluginRef{{Name: "demo", Git: u.bare, Ref: ref}}
	if err := cfg.Finalize(base); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	return &Manager{Config: cfg, LockPath: filepath.Join(base, LockFile), Out: &out}, &out
}

func TestSyncClonesLocksAndIsIdempotent(t *testing.T) {
	u := newUpstream(t)
	first := git(t, u.work, "rev-parse", "HEAD")
	m, out := setup(t, u, "")

	if err := m.Sync(context.Background(), nil, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "demo  cloned  "+first[:7]) || !strings.Contains(out.String(), "review") {
		t.Fatalf("out = %q", out.String())
	}
	dest := filepath.Join(m.Config.PluginDirs[0], "demo")
	if _, err := os.Stat(filepath.Join(dest, "init.lua")); err != nil {
		t.Fatal(err)
	}
	lock, err := ReadLock(m.LockPath)
	if err != nil {
		t.Fatal(err)
	}
	if lock["demo"] != (LockEntry{Git: u.bare, Commit: first}) {
		t.Fatalf("lock = %+v", lock)
	}
	raw1, _ := os.ReadFile(m.LockPath)

	// a new upstream commit does not move a locked plugin without --update
	second := u.commit("return {}\n")
	out.Reset()
	if err := m.Sync(context.Background(), nil, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "demo  unchanged  "+first[:7]) || strings.Contains(out.String(), "review") {
		t.Fatalf("out = %q", out.String())
	}
	raw2, _ := os.ReadFile(m.LockPath)
	if !bytes.Equal(raw1, raw2) {
		t.Fatalf("lock changed:\n%s\n%s", raw1, raw2)
	}

	out.Reset()
	if err := m.Sync(context.Background(), []string{"demo"}, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "demo  updated  "+second[:7]) {
		t.Fatalf("out = %q", out.String())
	}
	if got := git(t, dest, "rev-parse", "HEAD"); got != second {
		t.Fatalf("HEAD = %s, want %s", got, second)
	}
	lock, _ = ReadLock(m.LockPath)
	if lock["demo"].Commit != second {
		t.Fatalf("lock = %+v", lock)
	}
}

func TestSyncTagRef(t *testing.T) {
	u := newUpstream(t)
	tagged := git(t, u.work, "rev-parse", "HEAD")
	git(t, u.work, "tag", "v1")
	u.commit("return {}\n") // pushes the tag too; main moves past it
	m, _ := setup(t, u, "v1")
	if err := m.Sync(context.Background(), nil, false); err != nil {
		t.Fatal(err)
	}
	lock, _ := ReadLock(m.LockPath)
	if lock["demo"] != (LockEntry{Git: u.bare, Ref: "v1", Commit: tagged}) {
		t.Fatalf("lock = %+v", lock)
	}
}

func TestSyncBranchRefFollowsRemote(t *testing.T) {
	u := newUpstream(t)
	m, _ := setup(t, u, "main")
	if err := m.Sync(context.Background(), nil, false); err != nil {
		t.Fatal(err)
	}
	second := u.commit("return {}\n")
	if err := m.Sync(context.Background(), nil, true); err != nil {
		t.Fatal(err)
	}
	lock, _ := ReadLock(m.LockPath)
	if lock["demo"].Commit != second {
		t.Fatalf("lock = %+v, want %s", lock, second)
	}
}

func TestSyncRefusesForeignDir(t *testing.T) {
	u := newUpstream(t)
	m, _ := setup(t, u, "")
	dest := filepath.Join(m.Config.PluginDirs[0], "demo")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dest, "init.lua"), []byte("mine"), 0o644)
	err := m.Sync(context.Background(), nil, false)
	if err == nil || !strings.Contains(err.Error(), "not a git clone") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "init.lua")); string(b) != "mine" {
		t.Fatal("foreign dir was modified")
	}

	// a clone of a different URL is refused too
	other := newUpstream(t)
	_ = os.RemoveAll(dest)
	git(t, m.Config.PluginDirs[0], "clone", "-q", "--", other.bare, dest)
	err = m.Sync(context.Background(), nil, false)
	if err == nil || !strings.Contains(err.Error(), "clone of") {
		t.Fatalf("err = %v", err)
	}
}

func TestSyncUnknownName(t *testing.T) {
	u := newUpstream(t)
	m, _ := setup(t, u, "")
	if err := m.Sync(context.Background(), []string{"nope"}, false); err == nil {
		t.Fatal("expected error")
	}
}

func TestListAndRemove(t *testing.T) {
	u := newUpstream(t)
	m, out := setup(t, u, "")
	rows, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].State != StateMissing {
		t.Fatalf("rows = %+v", rows)
	}
	if err := m.Sync(context.Background(), nil, false); err != nil {
		t.Fatal(err)
	}
	head := git(t, u.work, "rev-parse", "HEAD")
	rows, _ = m.List()
	if rows[0].State != StateOK || rows[0].Installed != head || rows[0].Locked != head {
		t.Fatalf("rows = %+v", rows)
	}
	dest := filepath.Join(m.Config.PluginDirs[0], "demo")
	_ = os.WriteFile(filepath.Join(dest, "init.lua"), []byte("edited"), 0o644)
	if rows, _ = m.List(); rows[0].State != StateDirty {
		t.Fatalf("rows = %+v", rows)
	}
	git(t, dest, "checkout", "-q", "--", "init.lua")
	second := u.commit("return {}\n")
	git(t, dest, "fetch", "-q", "origin")
	git(t, dest, "checkout", "-q", "--detach", second)
	if rows, _ = m.List(); rows[0].State != StateMismatch {
		t.Fatalf("rows = %+v", rows)
	}

	out.Reset()
	if err := m.Remove("demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("dest still there: %v", err)
	}
	lock, _ := ReadLock(m.LockPath)
	if _, ok := lock["demo"]; ok {
		t.Fatal("lock entry kept")
	}
	if !strings.Contains(out.String(), "config") {
		t.Fatalf("no reminder: %q", out.String())
	}
	// not managed any more
	if err := m.Remove("demo"); err == nil {
		t.Fatal("expected error")
	}
}

func TestRemoveRefusesUnmanaged(t *testing.T) {
	u := newUpstream(t)
	m, _ := setup(t, u, "")
	dest := filepath.Join(m.Config.PluginDirs[0], "demo")
	_ = os.MkdirAll(dest, 0o755)
	if err := m.Remove("demo"); err == nil {
		t.Fatal("expected error")
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatal("unmanaged dir deleted")
	}
}

func TestSyncFollowsMovedTag(t *testing.T) {
	u := newUpstream(t)
	git(t, u.work, "tag", "v1")
	git(t, u.work, "push", "-q", "origin", "v1")
	m, _ := setup(t, u, "v1")
	if err := m.Sync(context.Background(), nil, false); err != nil {
		t.Fatal(err)
	}
	moved := u.commit("return {}\n")
	git(t, u.work, "tag", "-f", "v1")
	git(t, u.work, "push", "-q", "-f", "origin", "v1")
	if err := m.Sync(context.Background(), nil, true); err != nil {
		t.Fatal(err)
	}
	lock, _ := ReadLock(m.LockPath)
	if lock["demo"].Commit != moved {
		t.Fatalf("lock = %+v, want %s", lock, moved)
	}
}

func TestSyncRefusesExtTransport(t *testing.T) {
	u := newUpstream(t)
	m, _ := setup(t, u, "")
	marker := filepath.Join(t.TempDir(), "pwned")
	m.Config.Plugins[0].Git = "ext::sh -c touch% " + marker
	if err := m.Sync(context.Background(), nil, false); err == nil {
		t.Fatal("expected error")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("ext:: transport ran a command")
	}
}

func TestSyncRejectsInvalidLockedCommit(t *testing.T) {
	u := newUpstream(t)
	m, _ := setup(t, u, "")
	if err := WriteLock(m.LockPath, Lock{"demo": {Git: u.bare, Commit: "--orphan=x"}}); err != nil {
		t.Fatal(err)
	}
	err := m.Sync(context.Background(), nil, false)
	if err == nil || !strings.Contains(err.Error(), "invalid commit") {
		t.Fatalf("err = %v", err)
	}
}

func TestListForeignAndLockErrors(t *testing.T) {
	u := newUpstream(t)
	m, _ := setup(t, u, "")
	dest := filepath.Join(m.Config.PluginDirs[0], "demo")
	_ = os.MkdirAll(dest, 0o755)
	rows, err := m.List()
	if err != nil || rows[0].State != StateForeign {
		t.Fatalf("rows = %+v err = %v", rows, err)
	}
	_ = os.WriteFile(m.LockPath, []byte("{broken"), 0o644)
	if _, err := m.List(); err == nil {
		t.Fatal("expected lock read error")
	}
}
