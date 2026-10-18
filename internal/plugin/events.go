package plugin

import (
	"context"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// Event names accepted by mcp.on.
const (
	eventConfigReload = "config_reload"
	eventBeforeTool   = "before_tool"
	eventAfterTool    = "after_tool"
	eventShutdown     = "shutdown"
)

var knownEvents = map[string]bool{eventConfigReload: true, eventBeforeTool: true, eventAfterTool: true, eventShutdown: true}

// hookTimeout bounds one hook call (also capped by the plugin's own timeout).
var hookTimeout = 5 * time.Second

// luaOn implements mcp.on(event, fn). Hooks are staged on the instance like
// other registrations, so a failed plugin leaves none behind.
func (p *instance) luaOn(L *lua.LState) int {
	if !p.loading {
		L.RaiseError("mcp.on: only allowed while the plugin loads (top level or setup)")
		return 0
	}
	event := L.CheckString(1)
	fn := L.CheckFunction(2)
	if !knownEvents[event] {
		L.RaiseError("mcp.on: unknown event %q (want config_reload, before_tool, after_tool or shutdown)", event)
		return 0
	}
	if p.hooks == nil {
		p.hooks = map[string][]*lua.LFunction{}
	}
	p.hooks[event] = append(p.hooks[event], fn)
	return 0
}

// fire runs the plugin's hooks for event in registration order under the call
// lock. mkArg builds the argument inside the lock (the VM is single-threaded);
// each hook's results are handed to onResult. Failures are logged and skipped.
// It stops early when onResult returns true.
func (p *instance) fire(ctx context.Context, event string, mkArg func(L *lua.LState) lua.LValue, onResult func(res []lua.LValue) (stop bool)) {
	for _, fn := range p.hooks[event] {
		hctx, cancel := context.WithTimeout(ctx, min(hookTimeout, p.timeout))
		stop := false
		err := p.do(hctx, "hook "+event, func(ctx context.Context) error {
			var args []lua.LValue
			if mkArg != nil {
				args = append(args, mkArg(p.vm.L))
			}
			res, err := p.vm.call(ctx, fn, args...)
			if err != nil {
				return err
			}
			if onResult != nil {
				stop = onResult(res)
			}
			return nil
		})
		cancel()
		if err != nil && p.ctx.Err() == nil { // an unloaded plugin's skipped hook is not news
			p.log.Error("plugin "+p.name+": "+event+" hook ignored", err)
		}
		if stop {
			return
		}
	}
}

func (h *Host) snapshot() []*instance {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.plugins
}

// CheckTool runs the before_tool hooks in order. The first hook returning
// exactly false denies the call; plugin names the owner and reason is the
// second return value. Hook errors and timeouts allow the call.
func (h *Host) CheckTool(ctx context.Context, tool, args string) (allow bool, plugin, reason string) {
	for _, p := range h.snapshot() {
		denied := false
		p.fire(ctx, eventBeforeTool, toolArg(tool, args, nil), func(res []lua.LValue) bool {
			if len(res) > 0 && res[0] == lua.LFalse {
				denied = true
				if len(res) > 1 && res[1] != lua.LNil {
					reason = res[1].String()
				}
			}
			return denied
		})
		if denied {
			return false, p.name, reason
		}
	}
	return true, "", ""
}

// BeforeTool reports whether the call may proceed; see CheckTool.
func (h *Host) BeforeTool(ctx context.Context, tool, args string) (allow bool, reason string) {
	allow, _, reason = h.CheckTool(ctx, tool, args)
	return allow, reason
}

// AfterTool runs the after_tool hooks with the call's error (nil on success).
func (h *Host) AfterTool(ctx context.Context, tool, args string, err error) {
	for _, p := range h.snapshot() {
		p.fire(ctx, eventAfterTool, toolArg(tool, args, err), nil)
	}
}

// AfterToolAsync runs AfterTool in its own goroutine, detached from ctx's
// cancellation, so a slow hook never delays the tool's response and a
// cancelled request still reaches after_tool. One goroutine per call; each
// hook is bounded by hookTimeout. After Close it does nothing.
func (h *Host) AfterToolAsync(ctx context.Context, tool, args string, err error) {
	h.afterMu.Lock()
	if h.afterClosed {
		h.afterMu.Unlock()
		return
	}
	h.afterN++
	h.afterMu.Unlock()
	ctx = context.WithoutCancel(ctx)
	go func() {
		defer func() {
			h.afterMu.Lock()
			h.afterN--
			if h.afterN == 0 {
				h.afterIdle.Broadcast()
			}
			h.afterMu.Unlock()
		}()
		h.AfterTool(ctx, tool, args, err)
	}()
}

// WaitAfterTool waits until every after_tool dispatch started by
// AfterToolAsync so far has finished.
func (h *Host) WaitAfterTool() {
	h.afterMu.Lock()
	defer h.afterMu.Unlock()
	for h.afterN > 0 {
		h.afterIdle.Wait()
	}
}

func toolArg(tool, args string, err error) func(L *lua.LState) lua.LValue {
	return func(L *lua.LState) lua.LValue {
		t := L.NewTable()
		t.RawSetString("tool", lua.LString(tool))
		t.RawSetString("args", lua.LString(args))
		if err != nil {
			t.RawSetString("error", lua.LString(err.Error()))
		}
		return t
	}
}

// fireAll runs event hooks of the given instances (no result handling).
func fireAll(ps []*instance, event string, mkArg func(L *lua.LState) lua.LValue) {
	for _, p := range ps {
		p.fire(context.Background(), event, mkArg, nil)
	}
}

func configReloadArg(sources int) func(L *lua.LState) lua.LValue {
	return func(L *lua.LState) lua.LValue {
		t := L.NewTable()
		t.RawSetString("sources", lua.LNumber(sources))
		return t
	}
}
