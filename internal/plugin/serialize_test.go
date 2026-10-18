package plugin

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/skromesch/mcplogview-go/internal/config"
)

// slowBackend delays Exec for one host; entered signals the start of a slow
// call, slowEnd/otherStart record timings.
type slowBackend struct {
	*localBackend
	slowHost string
	delay    time.Duration
	entered  chan struct{}

	tmu        sync.Mutex
	slowEnd    time.Time
	otherStart time.Time
}

func (b *slowBackend) Exec(ctx context.Context, src config.Source, script string) ([]byte, error) {
	if src.Host != b.slowHost {
		b.tmu.Lock()
		if b.otherStart.IsZero() {
			b.otherStart = time.Now()
		}
		b.tmu.Unlock()
		return b.localBackend.Exec(ctx, src, script)
	}
	select {
	case b.entered <- struct{}{}:
	default:
	}
	defer func() {
		b.tmu.Lock()
		b.slowEnd = time.Now()
		b.tmu.Unlock()
	}()
	select {
	case <-time.After(b.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return b.localBackend.Exec(ctx, src, script)
}

// hostsPlugin lists children of hosts/<octet> by running a command on
// 127.0.0.<octet>.
const hostsPlugin = `return { api_version = 1, setup = function()
  mcp.source.register{ id = "hosts", label = "Hosts",
    children = function(path)
      if #path == 0 then return {} end
      mcp.ssh("127.0.0." .. path[1]):exec("true")
      return {}
    end,
    read = function() return { lines = {}, eof = true } end }
end }`

// A slow host delays other calls into the plugin (calls are serialized per
// plugin, documented) but no longer than its own call, and a queued caller
// whose ctx ends returns at once.
func TestSlowHostSerializesAndQueuedCallerCanCancel(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "hosts", hostsPlugin)
	lb := &localBackend{work: t.TempDir(), down: map[string]bool{}}
	sb := &slowBackend{localBackend: lb, slowHost: "127.0.0.1", delay: 300 * time.Millisecond, entered: make(chan struct{}, 1)}
	ref := config.PluginRef{
		Name:  "hosts",
		Allow: []string{config.CapSSH},
		SSH:   &config.PluginSSH{AllowedCIDRs: []string{"127.0.0.0/8"}, User: "u", IdentityFile: "/k"},
	}
	h, _ := hostWithSink(t, Options{SSH: sb}, plugins(dir, ref))
	if st := statusOf(t, h, "hosts"); !st.OK {
		t.Fatalf("load: %+v", st)
	}
	ctx := context.Background()

	aDone := make(chan error, 1)
	go func() { _, err := h.Children(ctx, "hosts/1"); aDone <- err }()
	<-sb.entered
	bStart := time.Now()
	if _, err := h.Children(ctx, "hosts/45"); err != nil {
		t.Fatal(err)
	}
	bDur := time.Since(bStart)
	if err := <-aDone; err != nil {
		t.Fatal(err)
	}
	sb.tmu.Lock()
	slowEnd, otherStart := sb.slowEnd, sb.otherStart
	sb.tmu.Unlock()
	if otherStart.Before(slowEnd) {
		t.Fatalf("host B ran before host A finished (B %v, A end %v)", otherStart, slowEnd)
	}
	if bDur > sb.delay+time.Second {
		t.Fatalf("host B took %v, more than A's %v plus margin", bDur, sb.delay)
	}

	sb.delay = time.Second
	a2Done := make(chan error, 1)
	go func() { _, err := h.Children(ctx, "hosts/1"); a2Done <- err }()
	<-sb.entered
	cctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := h.Children(cctx, "hosts/45")
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("queued caller returned after %v", d)
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("queued caller: want a ctx error, got %v", err)
	}
	<-a2Done
}
