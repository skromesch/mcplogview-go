package plugin

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"strings"

	lua "github.com/yuin/gopher-lua"

	"github.com/skromesch/mcplogview-go/internal/config"
	"github.com/skromesch/mcplogview-go/internal/logs"
)

const (
	// maxSSHCommand caps the script s:exec sends to the remote shell.
	maxSSHCommand = 8 << 10
	// maxSSHOutput caps the stdout s:exec hands back to the plugin. Larger
	// output is an "output too large" error rather than a silent truncation,
	// so a plugin never parses a cut-off result as if it were complete. The
	// real backend enforces it while streaming (logs.RunScriptLimit kills
	// ssh); the length check in luaExec covers any other backend.
	maxSSHOutput = logs.MaxSSHBytes
	// defaultTailLines is s:tail's max_lines when omitted (like the tools).
	defaultTailLines = 100
)

// SSHBackend runs the remote side of the ssh capability. The capability has
// already validated the host and arguments when it calls the backend.
type SSHBackend interface {
	Exec(ctx context.Context, src config.Source, script string) ([]byte, error)
	Tail(ctx context.Context, src config.Source, path string, offset int64, maxLines int) (logs.RangeResult, error)
}

// logsSSH is the real SSHBackend: the system ssh client via package logs.
type logsSSH struct{}

func (logsSSH) Exec(ctx context.Context, src config.Source, script string) ([]byte, error) {
	return logs.RunScriptLimit(ctx, src, script, maxSSHOutput)
}

func (logsSSH) Tail(ctx context.Context, src config.Source, path string, offset int64, maxLines int) (logs.RangeResult, error) {
	return logs.ReadSSHRange(ctx, src, path, offset, maxLines)
}

// sshCap is a plugin's ssh capability: the configured identity and the
// networks it may reach. Every check is done here, in Go; the plugin only
// chooses the host (within allowed) and the command.
type sshCap struct {
	backend  SSHBackend
	cfg      config.PluginSSH
	allowed  []netip.Prefix
	sourceID string
}

func newSSHCap(name string, cfg config.PluginSSH, backend SSHBackend) *sshCap {
	if backend == nil {
		backend = logsSSH{}
	}
	c := &sshCap{backend: backend, cfg: cfg, sourceID: "plugin-" + name}
	for _, s := range cfg.AllowedCIDRs {
		// Config validation already checked the CIDRs; one netip rejects
		// is skipped, which only ever narrows what is allowed.
		if p, err := netip.ParsePrefix(s); err == nil {
			c.allowed = append(c.allowed, p.Masked())
		}
	}
	return c
}

// checkHost accepts only a canonical IPv4 literal inside an allowed CIDR. No
// hostnames (they would let DNS decide where we connect), no IPv6, no zones.
func (c *sshCap) checkHost(host string) error {
	addr, err := netip.ParseAddr(host)
	// netip.ParseAddr is strict (no leading zeros, spaces or IPv4 zones);
	// Is4 keeps IPv6 out even when an IPv6 CIDR is configured.
	if err == nil && addr.Is4() {
		for _, p := range c.allowed {
			if p.Contains(addr) {
				return nil
			}
		}
	}
	return fmt.Errorf("mcp.ssh: host %q not allowed (want an IPv4 address in %s)", host, strings.Join(c.cfg.AllowedCIDRs, ", "))
}

// source is what the backend connects with: the configured user and key.
func (c *sshCap) source(host, path string) config.Source {
	port := c.cfg.Port
	if port == 0 {
		port = 22
	}
	return config.Source{
		Type: config.SourceSSH, ID: c.sourceID, Label: c.sourceID, Path: path,
		Host: host, User: c.cfg.User, IdentityFile: c.cfg.IdentityFile, Port: port,
		StrictHostKeyChecking: c.cfg.StrictHostKeyChecking, Multiplex: true,
	}
}

// remoteErr names the host in a backend error; ssh exits with 255 on its own
// failures (connection, authentication), reported as "cannot reach".
func remoteErr(host string, err error) string {
	var ee *exec.ExitError
	// ssh cannot tell its own exit 255 from a remote command exiting 255.
	if errors.As(err, &ee) && ee.ExitCode() == 255 {
		detail := strings.TrimPrefix(strings.TrimPrefix(err.Error(), ee.Error()), ": ")
		if detail == "" {
			detail = ee.Error()
		}
		return fmt.Sprintf("cannot reach %s: %s", host, detail)
	}
	return fmt.Sprintf("%s: %s", host, err.Error())
}

// luaSSH implements mcp.ssh(host): an object with exec and tail methods
// bound to the validated host.
func (c *sshCap) luaSSH(L *lua.LState) int {
	host := L.CheckString(1)
	if err := c.checkHost(host); err != nil {
		L.RaiseError("%s", err.Error())
		return 0
	}
	obj := L.NewTable()
	obj.RawSetString("exec", L.NewFunction(func(L *lua.LState) int { return c.luaExec(L, host) }))
	obj.RawSetString("tail", L.NewFunction(func(L *lua.LState) int { return c.luaTail(L, host) }))
	L.Push(obj)
	return 1
}

// luaExec implements s:exec(cmd) -> stdout.
func (c *sshCap) luaExec(L *lua.LState, host string) int {
	L.CheckTable(1)
	cmd := L.CheckString(2)
	if len(cmd) > maxSSHCommand {
		L.RaiseError("ssh exec: command too long (%d bytes, max %d)", len(cmd), maxSSHCommand)
		return 0
	}
	if strings.IndexByte(cmd, 0) >= 0 {
		L.RaiseError("ssh exec: command contains a NUL byte")
		return 0
	}
	out, err := c.backend.Exec(callCtx(L), c.source(host, "/"), cmd)
	if errors.Is(err, logs.ErrOutputTooLarge) || len(out) > maxSSHOutput {
		L.RaiseError("%s: output too large (limit %d MiB)", host, maxSSHOutput>>20)
		return 0
	}
	if err != nil {
		L.RaiseError("%s", remoteErr(host, err))
		return 0
	}
	L.Push(lua.LString(out))
	return 1
}

// luaTail implements s:tail(path, offset, max_lines) -> {lines, offset, size, eof}.
func (c *sshCap) luaTail(L *lua.LState, host string) int {
	L.CheckTable(1)
	path := L.CheckString(2)
	if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "\x00\n\r") {
		L.RaiseError("ssh tail: path must be absolute, without NUL or line breaks: %q", path)
		return 0
	}
	off := float64(L.CheckNumber(3))
	if off < 0 || off > 1<<53 || off != float64(int64(off)) {
		L.RaiseError("ssh tail: offset must be a non-negative integer")
		return 0
	}
	maxLines := min(max(L.OptInt(4, defaultTailLines), 1), maxReadLines)
	r, err := c.backend.Tail(callCtx(L), c.source(host, path), path, int64(off), maxLines)
	if err != nil {
		L.RaiseError("%s", remoteErr(host, err))
		return 0
	}
	lines := L.CreateTable(len(r.Lines), 0)
	for _, l := range r.Lines {
		lines.Append(lua.LString(l))
	}
	t := L.NewTable()
	t.RawSetString("lines", lines)
	t.RawSetString("offset", lua.LNumber(r.Offset))
	t.RawSetString("size", lua.LNumber(r.Size))
	t.RawSetString("eof", lua.LBool(r.EOF))
	L.Push(t)
	return 1
}

// callCtx is the context of the plugin call running L (set by vm.call), so
// timeouts and unloading cancel the remote command.
func callCtx(L *lua.LState) context.Context {
	if ctx := L.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}
