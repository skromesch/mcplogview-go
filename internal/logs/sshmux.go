package logs

import (
	"fmt"
	"os"
	"runtime"
	"sync"
)

var (
	muxOnce    sync.Once
	muxDirPath string
)

// muxDirFunc is muxDir; replaced in tests.
var muxDirFunc = muxDir

// muxDir returns the private control-socket directory, or "" when
// multiplexing must be skipped. The result is computed once.
func muxDir() string {
	muxOnce.Do(func() {
		if runtime.GOOS == "windows" {
			return
		}
		muxDirPath = prepareMuxDir(defaultMuxDir())
	})
	return muxDirPath
}

// defaultMuxDir is short on purpose: dir + "/" + the 40-byte %C hash must fit
// the 104-byte unix socket path limit.
func defaultMuxDir() string { return fmt.Sprintf("/tmp/mcplv-%d", os.Getuid()) }
