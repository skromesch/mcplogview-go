//go:build !windows

package logs

import (
	"os"
	"path/filepath"
	"testing"
)

// A directory planted between our Lstat and Mkdir (Mkdir then reports EEXIST)
// must not be chmod-ed: only its existing mode/owner decide, so a 0755 one
// falls back to no multiplexing and keeps its mode.
func TestPrepareMuxDirNoChmodOnRacedExistingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "raced")
	old := muxMkdir
	t.Cleanup(func() { muxMkdir = old })
	muxMkdir = func(name string, _ os.FileMode) error {
		if err := os.Mkdir(name, 0o755); err != nil {
			return err
		}
		if err := os.Chmod(name, 0o755); err != nil {
			return err
		}
		return os.Mkdir(name, 0o700) // EEXIST, as in the race
	}
	if got := prepareMuxDir(dir); got != "" {
		t.Fatalf("raced 0755 dir must fall back, got %q", got)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("existing dir was chmod-ed to %v", fi.Mode().Perm())
	}
}

// A directory we created ourselves is fixed up to 0700 and used.
func TestPrepareMuxDirFreshDirCreated0700(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fresh")
	if got := prepareMuxDir(dir); got != dir {
		t.Fatalf("fresh dir: %q", got)
	}
	fi, err := os.Lstat(dir)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("fresh dir mode: %v %v", fi, err)
	}
}
