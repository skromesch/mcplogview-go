// Package pluginmgr installs Lua plugins from git: it clones them into the
// first plugin directory, pins them in a lock file and removes them again.
package pluginmgr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/skromesch/mcplogview-go/internal/config"
)

// LockFile is the lock file's name, next to the config file.
const LockFile = "plugins.lock.json"

// gitTimeout bounds every git call.
const gitTimeout = 120 * time.Second

// gitEnv hardens every git call: no prompts, no ext:: or other exotic
// transports, also not through submodules or redirects.
var hardenedEnv = []string{
	"GIT_TERMINAL_PROMPT=0",
	"GIT_PROTOCOL_FROM_USER=0",
	"GIT_ALLOW_PROTOCOL=https:ssh:git:file",
	"GIT_CONFIG_COUNT=1",
	"GIT_CONFIG_KEY_0=protocol.ext.allow",
	"GIT_CONFIG_VALUE_0=never",
}

var commitRe = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// gitRun runs git in dir (when not empty) and returns stdout. Replaced in tests.
var gitRun = func(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), hardenedEnv...)
	cmd.WaitDelay = 2 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if ctx.Err() != nil {
			msg = "timed out after " + gitTimeout.String()
		}
		if msg == "" {
			return stdout.String(), fmt.Errorf("git %s: %w", args[0], err)
		}
		return stdout.String(), fmt.Errorf("git %s: %s", args[0], msg)
	}
	return stdout.String(), nil
}

// LockEntry pins one plugin to a commit.
type LockEntry struct {
	Git    string `json:"git"`
	Ref    string `json:"ref,omitempty"`
	Commit string `json:"commit"`
}

// Lock maps plugin names to their pinned commits.
type Lock map[string]LockEntry

// ReadLock reads a lock file; a missing file is an empty lock.
func ReadLock(path string) (Lock, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Lock{}, nil
	}
	if err != nil {
		return nil, err
	}
	lock := Lock{}
	if err := json.Unmarshal(b, &lock); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return lock, nil
}

// WriteLock writes the lock with sorted keys and stable indentation.
func WriteLock(path string, lock Lock) error {
	b, err := json.MarshalIndent(lock, "", "  ") // map keys are sorted
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".plugins.lock-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(append(b, '\n'))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, 0o644)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

// Manager syncs, lists and removes the git plugins of one config.
type Manager struct {
	Config   config.Config // finalized
	LockPath string
	Out      io.Writer
}

// Dir is where plugins are installed: the first plugin directory.
func (m *Manager) Dir() string { return m.Config.PluginDirs[0] }

func (m *Manager) dest(name string) string { return filepath.Join(m.Dir(), name) }

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// selected returns the git plugins, or the named ones (which must have git).
func (m *Manager) selected(names []string) ([]config.PluginRef, error) {
	if len(names) == 0 {
		var refs []config.PluginRef
		for _, p := range m.Config.Plugins {
			if p.Git != "" {
				refs = append(refs, p)
			}
		}
		return refs, nil
	}
	refs := make([]config.PluginRef, 0, len(names))
	for _, n := range names {
		found := false
		for _, p := range m.Config.Plugins {
			if p.Name == n {
				if p.Git == "" {
					return nil, fmt.Errorf("plugin %s has no git URL in the config", n)
				}
				refs = append(refs, p)
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("plugin %s is not in the config", n)
		}
	}
	return refs, nil
}

// Sync installs or updates the git plugins (all, or the named ones). Locked
// plugins stay on their commit unless update is set.
func (m *Manager) Sync(ctx context.Context, names []string, update bool) error {
	if _, err := exec.LookPath("git"); err != nil {
		return errors.New("git is not installed or not in PATH; plugin sync needs it")
	}
	refs, err := m.selected(names)
	if err != nil {
		return err
	}
	lock, err := ReadLock(m.LockPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.Dir(), 0o755); err != nil {
		return err
	}
	warned := false
	var errs []error
	for _, p := range refs {
		action, commit, err := m.syncOne(ctx, p, lock, update, &warned)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p.Name, err))
			fmt.Fprintf(m.Out, "%s  failed  %v\n", p.Name, err)
			continue
		}
		lock[p.Name] = LockEntry{Git: p.Git, Ref: p.Ref, Commit: commit}
		fmt.Fprintf(m.Out, "%s  %s  %s\n", p.Name, action, short(commit))
	}
	if len(refs) > 0 {
		if err := WriteLock(m.LockPath, lock); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) syncOne(ctx context.Context, p config.PluginRef, lock Lock, update bool, warned *bool) (action, commit string, err error) {
	dest := m.dest(p.Name)
	before := ""
	if _, statErr := os.Stat(dest); statErr == nil {
		if err := m.checkClone(ctx, dest, p.Git); err != nil {
			return "", "", err
		}
		if _, err := gitRun(ctx, dest, "fetch", "--force", "--tags", "--prune", "origin"); err != nil {
			return "", "", err
		}
		before, _ = revParse(ctx, dest, "HEAD")
	} else {
		if !*warned {
			fmt.Fprintln(m.Out, "warning: plugins run code with your user's rights; review a plugin before enabling it")
			*warned = true
		}
		if _, err := gitRun(ctx, m.Dir(), "clone", "--quiet", "--", p.Git, dest); err != nil {
			return "", "", err
		}
	}

	if e, ok := lock[p.Name]; ok && !update && e.Git == p.Git && e.Ref == p.Ref && e.Commit != "" {
		if !commitRe.MatchString(e.Commit) {
			return "", "", fmt.Errorf("lock file has an invalid commit %q; fix it or run sync --update", e.Commit)
		}
		commit = e.Commit
	} else if commit, err = resolveRef(ctx, dest, p.Ref); err != nil {
		return "", "", err
	}
	if _, err := gitRun(ctx, dest, "checkout", "--quiet", "--detach", commit); err != nil {
		return "", "", err
	}
	switch {
	case before == "":
		action = "cloned"
	case before != commit:
		action = "updated"
	default:
		action = "unchanged"
	}
	return action, commit, nil
}

func revParse(ctx context.Context, dir, rev string) (string, error) {
	out, err := gitRun(ctx, dir, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	return strings.TrimSpace(out), err
}

// resolveRef finds the commit for ref: a remote branch, a tag or a commit;
// an empty ref means the remote's HEAD.
func resolveRef(ctx context.Context, dir, ref string) (string, error) {
	if ref == "" {
		// follow a change of the remote's default branch
		_, _ = gitRun(ctx, dir, "remote", "set-head", "origin", "--auto")
	}
	candidates := []string{"refs/remotes/origin/HEAD"}
	if ref != "" {
		candidates = []string{"refs/remotes/origin/" + ref, "refs/tags/" + ref, ref}
	}
	for _, c := range candidates {
		if sha, err := revParse(ctx, dir, c); err == nil && sha != "" {
			return sha, nil
		}
	}
	if ref == "" {
		return "", errors.New("cannot determine the remote's default branch; set ref")
	}
	return "", fmt.Errorf("ref %q not found in the repository", ref)
}

// checkClone refuses dest unless it is a git clone of url.
func (m *Manager) checkClone(ctx context.Context, dest, url string) error {
	if _, err := os.Stat(filepath.Join(dest, ".git")); err != nil {
		return fmt.Errorf("%s exists but is not a git clone; move it away or remove it", dest)
	}
	out, err := gitRun(ctx, dest, "remote", "get-url", "origin")
	if err != nil {
		return fmt.Errorf("%s: %w", dest, err)
	}
	if got := strings.TrimSpace(out); got != url {
		return fmt.Errorf("%s is a clone of %s, not of %s; move it away or remove it", dest, got, url)
	}
	return nil
}

// Install states reported by List.
const (
	StateOK       = "ok"
	StateMissing  = "missing"
	StateDirty    = "dirty"
	StateMismatch = "mismatch" // installed commit differs from the lock
	StateForeign  = "foreign"  // not a git clone of the configured URL
	StateLocal    = "local"    // not installed from git
)

// Row describes one configured plugin.
type Row struct {
	Name      string
	Enabled   bool
	Git, Ref  string
	Locked    string // commit from the lock file
	Installed string // checked-out commit
	State     string
}

// List reports every configured plugin's install state.
func (m *Manager) List() ([]Row, error) {
	lock, err := ReadLock(m.LockPath)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	rows := make([]Row, 0, len(m.Config.Plugins))
	for _, p := range m.Config.Plugins {
		r := Row{Name: p.Name, Enabled: p.IsEnabled(), Git: p.Git, Ref: p.Ref, Locked: lock[p.Name].Commit}
		dest := m.dest(p.Name)
		switch {
		case p.Git == "":
			r.State = StateLocal
		case !exists(dest):
			r.State = StateMissing
		case m.checkClone(ctx, dest, p.Git) != nil:
			r.State = StateForeign
		default:
			r.Installed, _ = revParse(ctx, dest, "HEAD")
			status, err := gitRun(ctx, dest, "status", "--porcelain")
			switch {
			case err != nil || strings.TrimSpace(status) != "":
				r.State = StateDirty
			case r.Installed == "" || r.Installed != r.Locked:
				r.State = StateMismatch
			default:
				r.State = StateOK
			}
		}
		rows = append(rows, r)
	}
	return rows, nil
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Remove deletes a plugin installed by Sync and its lock entry. It does not
// edit the config.
func (m *Manager) Remove(name string) error {
	lock, err := ReadLock(m.LockPath)
	if err != nil {
		return err
	}
	e, ok := lock[name]
	if !ok {
		return fmt.Errorf("plugin %s was not installed by sync (no entry in %s)", name, m.LockPath)
	}
	dest := m.dest(name)
	if exists(dest) {
		if err := m.checkClone(context.Background(), dest, e.Git); err != nil {
			return err
		}
		if err := os.RemoveAll(dest); err != nil {
			return err
		}
	}
	delete(lock, name)
	if err := WriteLock(m.LockPath, lock); err != nil {
		return err
	}
	fmt.Fprintf(m.Out, "removed %s; remove it from \"plugins\" in the config too, or the next sync installs it again\n", dest)
	return nil
}
