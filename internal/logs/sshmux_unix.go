//go:build !windows

package logs

import (
	"os"
	"syscall"
)

// muxMkdir is os.Mkdir; replaced in tests to simulate a racing creator.
var muxMkdir = os.Mkdir

// prepareMuxDir makes sure dir is a real directory, mode 0700 and owned by
// the current user, creating it when missing. It returns "" (no multiplexing)
// if the directory is a symlink or has other ownership or permissions.
func prepareMuxDir(dir string) string {
	if _, err := os.Lstat(dir); os.IsNotExist(err) {
		err := muxMkdir(dir, 0o700)
		if err != nil && !os.IsExist(err) {
			return ""
		}
		if err == nil {
			// Only fix up a directory we created ourselves (undo umask
			// narrowing). An EEXIST one appeared after the Lstat (maybe
			// planted, maybe a symlink) and is judged as found below.
			_ = os.Chmod(dir, 0o700)
		}
	}
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 || fi.Mode().Perm() != 0o700 {
		return ""
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Getuid() {
		return ""
	}
	return dir
}
