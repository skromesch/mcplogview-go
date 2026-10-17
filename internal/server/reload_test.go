package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/skromesch/mcplogview-go/internal/config"
	"github.com/skromesch/mcplogview-go/internal/serverlog"
)

type captureLogger struct {
	serverlog.Logger
	lines chan string
}

func (c captureLogger) Info(m string) {
	select {
	case c.lines <- "INFO " + m:
	default:
	}
}
func (c captureLogger) Error(m string, e error) {
	select {
	case c.lines <- "ERROR " + m:
	default:
	}
}

func listIDs(t *testing.T, cs *mcp.ClientSession) string {
	t.Helper()
	text, isErr := call(t, cs, "list_logs", nil)
	if isErr {
		t.Fatal(text)
	}
	return text
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func writeCfg(t *testing.T, path, body string) {
	t.Helper()
	// write+rename like the configurator does, so the inode changes
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func TestRunningServerPicksUpEnableDisable(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "x.log")
	_ = os.WriteFile(logFile, []byte("a\n"), 0o644)
	cfgPath := filepath.Join(dir, "mcplogview.json")
	body := func(aDisabled, bDisabled string) string {
		return `{"logging":{"enabled":false},"sources":[
			{"id":"a","label":"A","path":"` + logFile + `"` + aDisabled + `},
			{"id":"b","label":"B","path":"` + logFile + `"` + bDisabled + `}]}`
	}
	writeCfg(t, cfgPath, body("", `,"disabled":true`))

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	logger := captureLogger{Logger: serverlog.Noop(), lines: make(chan string, 50)}
	rt := NewRuntime(cfg, logger)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go WatchConfig(ctx, rt, cfgPath, 30*time.Millisecond)

	st, ct := mcp.NewInMemoryTransports()
	if _, err := New(rt).Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	has := func(id string) bool { return strings.Contains(listIDs(t, cs), `"id":"`+id+`"`) }
	if !has("a") || has("b") {
		t.Fatalf("start state wrong: %s", listIDs(t, cs))
	}
	// a disabled source is not readable either
	if text, isErr := call(t, cs, "read_log_chunk", map[string]any{"sourceId": "b"}); !isErr || !strings.Contains(text, "unknown source") {
		t.Fatalf("disabled source must be unknown to agents: %q %v", text, isErr)
	}

	// switch: a off, b on -- while the session stays connected
	writeCfg(t, cfgPath, body(`,"disabled":true`, ""))
	waitFor(t, "b appears and a disappears", func() bool { return has("b") && !has("a") })
	if text, isErr := call(t, cs, "read_log_chunk", map[string]any{"sourceId": "b"}); isErr || !strings.Contains(text, `"lines":["a"]`) {
		t.Fatalf("newly enabled source must be readable: %q %v", text, isErr)
	}

	// a broken file must not take the service down: previous config stays
	writeCfg(t, cfgPath, `{ this is not json`)
	waitFor(t, "reload error is logged", func() bool {
		for {
			select {
			case l := <-logger.lines:
				if strings.HasPrefix(l, "ERROR config reload failed") {
					return true
				}
			default:
				return false
			}
		}
	})
	if !has("b") || has("a") {
		t.Fatalf("config must stay as before a broken edit: %s", listIDs(t, cs))
	}

	// and a fixed file is picked up again
	writeCfg(t, cfgPath, body("", ""))
	waitFor(t, "both enabled after the file is fixed", func() bool { return has("a") && has("b") })
}

func TestWatchConfigStopsOnContextCancel(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.json")
	writeCfg(t, p, `{"logging":{"enabled":false}}`)
	cfg, _ := config.Load(p)
	rt := NewRuntime(cfg, serverlog.Noop())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { WatchConfig(ctx, rt, p, 10*time.Millisecond); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not stop")
	}
}
