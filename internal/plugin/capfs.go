package plugin

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	lua "github.com/yuin/gopher-lua"

	"github.com/skromesch/mcplogview-go/internal/config"
)

// maxFSRead caps one mcp.fs.read call (also its default size).
const maxFSRead = 1 << 20

// maxFSEntries caps how many entries mcp.fs.list returns.
const maxFSEntries = 10000

// fsCap is a plugin's read-only filesystem capability. Containment is decided
// here, in Go: the argument is cleaned, symlinks are resolved, and the result
// must lie in (or be) one of the configured roots, themselves resolved.
//
// Local filesystem calls are fast and cannot be cancelled mid-syscall, so
// this capability does no logging and takes no call context.
//
// Known gap: between EvalSymlinks and the open/readdir the tree can change
// (TOCTOU). Accepted: roots are chosen by the admin and plugins are installed
// by the admin, so nobody untrusted can race the filesystem here.
type fsCap struct {
	roots []string
}

func newFSCap(cfg config.PluginFS) *fsCap {
	return &fsCap{roots: cfg.Roots}
}

// inside reports whether the resolved path p equals or lies under a resolved
// root. Roots are resolved on each call, so a root created after load works;
// a root that cannot be resolved matches nothing (only narrows access).
func (c *fsCap) inside(p string) bool {
	for _, r := range c.roots {
		rr, err := filepath.EvalSymlinks(r)
		if err != nil {
			continue
		}
		if under(p, rr) {
			return true
		}
	}
	return false
}

// lexicallyInside reports whether the cleaned, unresolved path lies under a
// root as configured or as resolved.
func (c *fsCap) lexicallyInside(clean string) bool {
	for _, r := range c.roots {
		if under(clean, filepath.Clean(r)) {
			return true
		}
		if rr, err := filepath.EvalSymlinks(r); err == nil && under(clean, rr) {
			return true
		}
	}
	return false
}

// under reports whether p equals root or is inside it (separator-aware).
func under(p, root string) bool {
	return p == root || strings.HasPrefix(p, strings.TrimSuffix(root, string(filepath.Separator))+string(filepath.Separator))
}

// resolve validates the plugin-supplied path and returns its symlink-free
// form inside a root. Errors name the argument, never a symlink's target.
func (c *fsCap) resolve(fn, arg string) (string, error) {
	if strings.IndexByte(arg, 0) >= 0 {
		return "", errors.New("mcp.fs." + fn + ": path contains a NUL byte")
	}
	if !filepath.IsAbs(arg) {
		return "", errors.New("mcp.fs." + fn + ": path must be absolute: " + quoteArg(arg))
	}
	clean := filepath.Clean(arg)
	p, err := filepath.EvalSymlinks(clean)
	if err != nil {
		// Only a path lexically under a root may learn why it failed; any
		// other path gets the containment error, so existence elsewhere on
		// disk is not observable.
		if !c.lexicallyInside(clean) {
			return "", errors.New("mcp.fs." + fn + ": path outside the allowed roots: " + quoteArg(arg))
		}
		if errors.Is(err, fs.ErrNotExist) {
			return "", errors.New("mcp.fs." + fn + ": no such file or directory: " + quoteArg(arg))
		}
		return "", errors.New("mcp.fs." + fn + ": cannot access " + quoteArg(arg))
	}
	if !c.inside(p) {
		return "", errors.New("mcp.fs." + fn + ": path outside the allowed roots: " + quoteArg(arg))
	}
	return p, nil
}

func quoteArg(s string) string { return strconv.Quote(s) }

// luaList implements mcp.fs.list(dir) -> array of {name, dir}, sorted by name.
func (c *fsCap) luaList(L *lua.LState) int {
	arg := L.CheckString(1)
	p, err := c.resolve("list", arg)
	if err != nil {
		L.RaiseError("%s", err.Error())
		return 0
	}
	if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
		L.RaiseError("mcp.fs.list: not a directory: %s", quoteArg(arg))
		return 0
	}
	d, err := os.Open(p)
	if err != nil {
		L.RaiseError("mcp.fs.list: cannot read %s", quoteArg(arg))
		return 0
	}
	defer d.Close()
	var entries []fs.DirEntry
	for {
		batch, err := d.ReadDir(1000)
		entries = append(entries, batch...)
		if len(entries) > maxFSEntries {
			L.RaiseError("mcp.fs.list: directory too large (limit %d entries)", maxFSEntries)
			return 0
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			L.RaiseError("mcp.fs.list: cannot read %s", quoteArg(arg))
			return 0
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	out := L.CreateTable(len(entries), 0)
	for _, e := range entries {
		isDir := e.IsDir()
		if e.Type()&fs.ModeSymlink != 0 {
			// Follow a link only if its target is inside a root.
			isDir = false
			if t, err := filepath.EvalSymlinks(filepath.Join(p, e.Name())); err == nil && c.inside(t) {
				if fi, err := os.Stat(t); err == nil && fi.IsDir() {
					isDir = true
				}
			}
		}
		row := L.NewTable()
		row.RawSetString("name", lua.LString(e.Name()))
		row.RawSetString("dir", lua.LBool(isDir))
		out.Append(row)
	}
	L.Push(out)
	return 1
}

// luaRead implements mcp.fs.read(path, offset, max_bytes) -> {data, size}.
func (c *fsCap) luaRead(L *lua.LState) int {
	arg := L.CheckString(1)
	off := float64(L.OptNumber(2, 0))
	if off < 0 || off > 1<<53 || off != float64(int64(off)) {
		L.RaiseError("mcp.fs.read: offset must be a non-negative integer")
		return 0
	}
	n := float64(L.OptNumber(3, maxFSRead))
	if n < 1 || n > maxFSRead || n != float64(int64(n)) {
		L.RaiseError("mcp.fs.read: max_bytes must be between 1 and %d", maxFSRead)
		return 0
	}
	p, err := c.resolve("read", arg)
	if err != nil {
		L.RaiseError("%s", err.Error())
		return 0
	}
	// Check the type before opening: open(2) on a FIFO blocks forever and on
	// a device node can have side effects. Then open without following links
	// and without blocking, and confirm it is still the file we checked.
	pre, err := os.Stat(p)
	if err != nil || !pre.Mode().IsRegular() {
		L.RaiseError("mcp.fs.read: not a regular file: %s", quoteArg(arg))
		return 0
	}
	f, err := os.OpenFile(p, os.O_RDONLY|openExtraFlags, 0)
	if err != nil {
		L.RaiseError("mcp.fs.read: cannot open %s", quoteArg(arg))
		return 0
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || !os.SameFile(pre, fi) {
		L.RaiseError("mcp.fs.read: not a regular file: %s", quoteArg(arg))
		return 0
	}
	size := fi.Size()
	var data []byte
	if int64(off) < size {
		want := min(int64(n), size-int64(off))
		data = make([]byte, want)
		got, err := io.ReadFull(io.NewSectionReader(f, int64(off), want), data)
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
			L.RaiseError("mcp.fs.read: cannot read %s", quoteArg(arg))
			return 0
		}
		data = data[:got]
	}
	t := L.NewTable()
	t.RawSetString("data", lua.LString(data))
	t.RawSetString("size", lua.LNumber(size))
	L.Push(t)
	return 1
}

// install adds mcp.fs.
func (c *fsCap) install(L *lua.LState, mcp *lua.LTable) {
	t := L.NewTable()
	t.RawSetString("list", L.NewFunction(c.luaList))
	t.RawSetString("read", L.NewFunction(c.luaRead))
	mcp.RawSetString("fs", t)
}
