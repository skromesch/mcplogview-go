package server

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"time"

	"github.com/skromesch/mcplogview-go/internal/config"
)

// WatchConfig polls the config file and applies source/skills changes to rt
// without a restart, so enabling or disabling a source takes effect on a
// running (e.g. central, long-lived) instance. A broken file is logged and the
// previous configuration stays active. Transport and logging settings need a
// restart; a note is logged when they changed.
func WatchConfig(ctx context.Context, rt *Runtime, path string, interval time.Duration) {
	last, _ := os.Stat(path)
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		st, err := os.Stat(path)
		if err != nil {
			continue // briefly missing during an editor's save; try again next tick
		}
		if last != nil && os.SameFile(last, st) && st.ModTime().Equal(last.ModTime()) && st.Size() == last.Size() {
			continue
		}
		last = st

		cfg, err := config.Load(path)
		if err != nil {
			rt.Logger.Error("config reload failed, keeping previous configuration", err)
			continue
		}
		old := rt.Config()
		rt.Apply(cfg)
		rt.Logger.Info(fmt.Sprintf("config reloaded: %d sources (%d enabled)", len(cfg.Sources), len(cfg.EnabledSources())))
		if !reflect.DeepEqual(old.Transport, cfg.Transport) || !reflect.DeepEqual(old.Logging, cfg.Logging) {
			rt.Logger.Info("transport/logging changes need a restart to take effect")
		}
	}
}
