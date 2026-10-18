//go:build unix

package plugin

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestFSFifoDoesNotHang(t *testing.T) {
	root, _ := fsFixture(t)
	if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	tl := fsTool(t, true, []string{root}, `
		if a.list then
			local n = 0
			for _ in ipairs(mcp.fs.list(a.p)) do n = n + 1 end
			return { n = n }
		end
		return mcp.fs.read(a.p)`)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := callX(t, tl, map[string]any{"p": root + "/pipe"})
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("fifo read: %v", err)
		}
		m, err := callX(t, tl, map[string]any{"p": root, "list": true})
		if err != nil || m["n"] != float64(7) {
			t.Errorf("list: %v %v", m, err)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("fs.read on a FIFO hung")
	}
}
