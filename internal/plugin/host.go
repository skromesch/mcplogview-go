package plugin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	lua "github.com/yuin/gopher-lua"

	"github.com/skromesch/mcplogview-go/internal/config"
	"github.com/skromesch/mcplogview-go/internal/serverlog"
)

// apiVersion is the plugin API version this host implements.
const apiVersion = 1

// defaultTimeout bounds a plugin call when PluginRef.TimeoutSeconds is 0.
const defaultTimeout = 120 * time.Second

// Options configures a Host.
type Options struct {
	// Logger receives host messages and mcp.log output (nil: discard).
	Logger serverlog.Logger
	// ReservedToolNames are tool names plugins must not register (the
	// server's built-in tools).
	ReservedToolNames []string
	// SSH runs the ssh capability's remote calls (nil: the system ssh client).
	SSH SSHBackend
}

// PluginStatus reports whether an enabled plugin loaded.
type PluginStatus struct {
	Name  string
	OK    bool
	Error string
}

// Host loads Lua plugins and exposes what they register.
type Host struct {
	log      serverlog.Logger
	reserved []string
	ssh      SSHBackend

	reloadMu   sync.Mutex // serializes Reload and Close
	loadedOnce bool       // a Reload has run; guarded by reloadMu
	shutDown   bool       // shutdown hooks fired; guarded by reloadMu

	mu      sync.RWMutex
	plugins []*instance
	status  []PluginStatus
	order   []*source
	roots   map[string]*source
	skills  map[string]string
	tools   []*tool

	// sinkMu serializes sink updates with the swap of h.tools, so the sink
	// always mirrors the published tools. Lock order: sinkMu, then mu.
	sinkMu sync.Mutex
	sink   ToolSink
	added  []string // names added to sink

	// In-flight AfterToolAsync dispatches; Close waits for them.
	afterMu     sync.Mutex
	afterIdle   *sync.Cond // signalled when afterN drops to 0
	afterN      int
	afterClosed bool // Close ran: no new dispatches
}

// NewHost returns a Host with no plugins loaded.
func NewHost(o Options) *Host {
	log := o.Logger
	if log == nil {
		log = serverlog.Noop()
	}
	h := &Host{log: log, reserved: o.ReservedToolNames, ssh: o.SSH}
	h.afterIdle = sync.NewCond(&h.afterMu)
	return h
}

// instance is one loaded plugin: its VM and everything it registered.
// Registrations are collected on the instance while it loads and only
// published to the Host when loading succeeded, so a failed plugin is rolled
// back by discarding the instance.
type instance struct {
	name    string
	timeout time.Duration
	vm      *vm
	log     serverlog.Logger
	ssh     *sshCap // nil unless the config allows ssh
	fs      *fsCap  // nil unless the config allows fs

	ctx    context.Context // cancelled when the plugin is unloaded
	cancel context.CancelFunc

	// sem is a 1-slot semaphore serializing calls, including result
	// conversion. Unlike a mutex, waiting for it can be abandoned.
	sem    chan struct{}
	closed bool // guarded by sem

	// Load-time state, touched only by the loading goroutine.
	loading bool
	taken   map[string]string // root id -> owner, from earlier sources/plugins
	sources []*source
	skills  map[string]string
	tools   []*tool
	hooks   map[string][]*lua.LFunction // event -> hooks, in registration order
	// toolTaken maps tool names used by built-ins and earlier plugins to the owner.
	toolTaken map[string]string
}

// Reload unloads every plugin and loads the enabled ones of cfg. Failures are
// reported through Status and the log; they never affect other plugins.
func (h *Host) Reload(cfg config.Config) {
	h.reloadMu.Lock()
	defer h.reloadMu.Unlock()

	taken := make(map[string]string, len(cfg.Sources))
	for _, s := range cfg.Sources {
		taken[s.ID] = "configured source " + s.ID
	}
	var (
		loaded []*instance
		status []PluginStatus
	)
	toolTaken := make(map[string]string, len(h.reserved))
	for _, n := range h.reserved {
		toolTaken[n] = "built-in tool " + n
	}
	var tools []*tool
	roots := map[string]*source{}
	skills := map[string]string{}
	var order []*source
	for _, ref := range cfg.Plugins {
		if !ref.IsEnabled() {
			continue
		}
		p, err := h.load(ref, cfg.PluginDirs, taken, toolTaken)
		if err != nil {
			h.log.Error("plugin "+ref.Name+": load failed", err)
			status = append(status, PluginStatus{Name: ref.Name, Error: err.Error()})
			continue
		}
		for _, s := range p.sources {
			taken[s.root.ID] = "plugin " + p.name
			roots[s.root.ID] = s
			order = append(order, s)
		}
		for id, text := range p.skills {
			skills[id] = text
		}
		for _, t := range p.tools {
			toolTaken[t.def.Name] = "plugin " + p.name
			tools = append(tools, t)
		}
		loaded = append(loaded, p)
		status = append(status, PluginStatus{Name: ref.Name, OK: true})
		h.log.Info(fmt.Sprintf("plugin %s: loaded (%d sources, %d tools)", p.name, len(p.sources), len(p.tools)))
	}
	h.swap(loaded, status, order, roots, skills, tools)
	first := !h.loadedOnce
	h.loadedOnce = true
	if !first {
		fireAll(loaded, eventConfigReload, configReloadArg(len(cfg.EnabledSources())+len(order)))
	}
}

// Close fires the shutdown hooks (once) and unloads every plugin. Pending
// after_tool dispatches are aborted with their plugins and waited for, so no
// hook goroutine outlives Close.
func (h *Host) Close() {
	h.afterMu.Lock()
	h.afterClosed = true
	h.afterMu.Unlock()
	h.reloadMu.Lock()
	defer h.reloadMu.Unlock()
	if !h.shutDown {
		h.shutDown = true
		fireAll(h.snapshot(), eventShutdown, nil)
	}
	h.swap(nil, nil, nil, nil, nil, nil)
	h.WaitAfterTool()
}

func (h *Host) swap(loaded []*instance, status []PluginStatus, order []*source, roots map[string]*source, skills map[string]string, tools []*tool) {
	h.sinkMu.Lock()
	if h.sink != nil {
		for _, n := range h.added {
			h.sink.Remove(n)
		}
	}
	h.mu.Lock()
	old := h.plugins
	h.plugins, h.status, h.order, h.roots, h.skills, h.tools = loaded, status, order, roots, skills, tools
	h.mu.Unlock()
	h.addToSink(tools)
	h.sinkMu.Unlock()
	for _, p := range old {
		p.close()
	}
}

// Status lists the enabled plugins of the last Reload in config order.
func (h *Host) Status() []PluginStatus {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return append([]PluginStatus(nil), h.status...)
}

// Skill returns the skill text a plugin registered for a root id.
func (h *Host) Skill(rootID string) (string, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	text, ok := h.skills[rootID]
	return text, ok
}

// load finds, runs and sets up one plugin. On any error the instance is
// closed and nothing it registered survives.
func (h *Host) load(ref config.PluginRef, dirs []string, taken, toolTaken map[string]string) (*instance, error) {
	path, chunk, err := findPlugin(dirs, ref.Name)
	if err != nil {
		return nil, fmt.Errorf("plugin %s: %w", ref.Name, err)
	}
	p := h.newInstance(ref, taken, toolTaken)
	if err := p.loadFile(path, chunk, ref.Opts); err != nil {
		p.close()
		return nil, err
	}
	p.loading = false
	p.taken, p.toolTaken = nil, nil
	return p, nil
}

func (h *Host) newInstance(ref config.PluginRef, taken, toolTaken map[string]string) *instance {
	timeout := time.Duration(ref.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &instance{
		name:      ref.Name,
		timeout:   timeout,
		vm:        newVM(),
		log:       h.log,
		ctx:       ctx,
		cancel:    cancel,
		sem:       make(chan struct{}, 1),
		loading:   true,
		taken:     taken,
		toolTaken: toolTaken,
		skills:    map[string]string{},
	}
	if ref.Allows(config.CapSSH) && ref.SSH != nil {
		p.ssh = newSSHCap(ref.Name, *ref.SSH, h.ssh)
	}
	if ref.Allows(config.CapFS) && ref.FS != nil {
		p.fs = newFSCap(*ref.FS)
	}
	p.installAPI()
	return p
}

// loadFile runs the plugin file and its setup. chunk names the file in
// error positions (relative to its plugin dir, so absolute paths of the
// host never reach agents).
func (p *instance) loadFile(path, chunk string, opts map[string]any) error {
	var mod *lua.LTable
	err := p.do(context.Background(), "load", func(ctx context.Context) error {
		v, err := p.vm.runFileAs(ctx, path, chunk)
		if err != nil {
			return err
		}
		t, ok := v.(*lua.LTable)
		if !ok {
			return fmt.Errorf("plugin file must return a table with api_version = %d", apiVersion)
		}
		if got := t.RawGetString("api_version"); got != lua.LNumber(apiVersion) {
			return fmt.Errorf("unsupported api_version %s (want %d)", got.String(), apiVersion)
		}
		mod = t
		return nil
	})
	if err != nil {
		return err
	}
	setup := mod.RawGetString("setup")
	if setup == lua.LNil {
		return nil
	}
	if setup.Type() != lua.LTFunction {
		return fmt.Errorf("plugin %s: setup must be a function", p.name)
	}
	return p.do(context.Background(), "setup", func(ctx context.Context) error {
		_, err := p.vm.call(ctx, setup, toLua(p.vm.L, opts))
		return err
	})
}

// findPlugin returns the first <dir>/<name>.lua or <dir>/<name>/init.lua and
// its chunk name ("<name>.lua" or "<name>/init.lua").
func findPlugin(dirs []string, name string) (path, chunk string, err error) {
	for _, d := range dirs {
		for _, c := range []string{name + ".lua", name + "/init.lua"} {
			p := filepath.Join(d, filepath.FromSlash(c))
			if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
				return p, c, nil
			}
		}
	}
	return "", "", fmt.Errorf("not found in pluginDirs (%s)", strings.Join(dirs, ", "))
}

// do runs f with the plugin's call timeout, serialized with other calls into
// the plugin. Errors (including Go panics) come back prefixed "plugin <name>: <op>: ".
func (p *instance) do(ctx context.Context, op string, f func(ctx context.Context) error) (err error) {
	cctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	// The timeout also bounds the wait behind a long-running call.
	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
	case <-cctx.Done():
		return p.callErr(ctx, cctx, op, cctx.Err())
	case <-p.ctx.Done():
		return fmt.Errorf("plugin %s: %s: unloaded", p.name, op)
	}
	if p.closed {
		return fmt.Errorf("plugin %s: %s: unloaded", p.name, op)
	}
	stop := context.AfterFunc(p.ctx, cancel)
	defer stop()
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
		if err != nil {
			err = p.callErr(ctx, cctx, op, err)
		}
	}()
	return f(cctx)
}

// callErr prefixes err with the plugin and operation, naming the unloaded and
// timed-out cases.
func (p *instance) callErr(ctx, cctx context.Context, op string, err error) error {
	switch {
	case p.ctx.Err() != nil:
		err = errors.New("unloaded")
	case ctx.Err() == nil && errors.Is(cctx.Err(), context.DeadlineExceeded):
		err = fmt.Errorf("timed out after %s", p.timeout)
	}
	return fmt.Errorf("plugin %s: %s: %w", p.name, op, err)
}

// close aborts a running call and frees the VM.
func (p *instance) close() {
	p.cancel()
	p.sem <- struct{}{} // the running call, if any, was just cancelled
	defer func() { <-p.sem }()
	if !p.closed {
		p.closed = true
		p.vm.L.Close()
	}
}
