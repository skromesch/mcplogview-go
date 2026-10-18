package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	lua "github.com/yuin/gopher-lua"

	"github.com/skromesch/mcplogview-go/internal/config"
)

// defaultCaseTimeout bounds one test case when TestOptions.CaseTimeout is 0.
const defaultCaseTimeout = 2 * time.Minute

// TestOptions configures RunTests.
type TestOptions struct {
	// Out receives one line per case and the summary.
	Out io.Writer
	// Run, when set, selects the cases whose name or file name matches.
	Run *regexp.Regexp
	// ReservedToolNames are passed to every Host the tests load.
	ReservedToolNames []string
	// CaseTimeout bounds one case (0: two minutes).
	CaseTimeout time.Duration
}

// TestResult counts the cases RunTests ran.
type TestResult struct {
	Passed, Failed int
}

// RunTests runs every <pluginDir>/tests/*_test.lua. Each file runs in a fresh
// sandboxed VM (the plugin sandbox) with a global test table; cases defined
// with test.case run one after another, and a failing case does not stop the
// others. The error is only for problems running the suite at all (no tests).
func RunTests(ctx context.Context, pluginDir string, o TestOptions) (TestResult, error) {
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.CaseTimeout <= 0 {
		o.CaseTimeout = defaultCaseTimeout
	}
	abs, err := filepath.Abs(pluginDir)
	if err != nil {
		return TestResult{}, err
	}
	files, err := filepath.Glob(filepath.Join(abs, "tests", "*_test.lua"))
	if err != nil {
		return TestResult{}, err
	}
	sort.Strings(files)
	if len(files) == 0 {
		return TestResult{}, fmt.Errorf("no tests found (%s)", filepath.Join(pluginDir, "tests", "*_test.lua"))
	}
	var res TestResult
	for _, f := range files {
		r := &testFile{opts: o, pluginDir: abs, file: filepath.Base(f)}
		r.run(ctx, f, &res)
	}
	fmt.Fprintf(o.Out, "%d passed, %d failed\n", res.Passed, res.Failed)
	return res, nil
}

type testCase struct {
	name string
	fn   *lua.LFunction
}

// testFile runs one test file.
type testFile struct {
	opts      TestOptions
	pluginDir string
	file      string
	cases     []testCase
	cur       *caseState // the running case
}

// caseState is what a case created; it is discarded after the case.
type caseState struct {
	backend *localBackend
	hosts   []*Host
	dirs    []string
}

func (r *testFile) run(ctx context.Context, path string, res *TestResult) {
	v := newVM()
	defer v.L.Close()
	r.install(v.L)
	if _, err := v.runFileAs(ctx, path, "tests/"+r.file); err != nil {
		res.Failed++
		fmt.Fprintf(r.opts.Out, "FAIL %s: (load): %s\n", r.file, oneLine(err.Error()))
		return
	}
	for _, c := range r.cases {
		if r.opts.Run != nil && !r.opts.Run.MatchString(c.name) && !r.opts.Run.MatchString(r.file) {
			continue
		}
		if err := r.runCase(ctx, v, c); err != nil {
			res.Failed++
			fmt.Fprintf(r.opts.Out, "FAIL %s: %s: %s\n", r.file, c.name, oneLine(err.Error()))
		} else {
			res.Passed++
			fmt.Fprintf(r.opts.Out, "ok   %s: %s\n", r.file, c.name)
		}
	}
}

func (r *testFile) runCase(ctx context.Context, v *vm, c testCase) error {
	work, err := os.MkdirTemp("", "mcplogview-test-")
	if err != nil {
		return err
	}
	st := &caseState{backend: &localBackend{work: work, down: map[string]bool{}}, dirs: []string{work}}
	r.cur = st
	defer func() {
		r.cur = nil
		for _, h := range st.hosts {
			h.Close()
		}
		for _, d := range st.dirs {
			os.RemoveAll(d)
		}
	}()
	cctx, cancel := context.WithTimeout(ctx, r.opts.CaseTimeout)
	defer cancel()
	_, err = v.call(cctx, c.fn)
	if err != nil && cctx.Err() != nil && ctx.Err() == nil {
		return fmt.Errorf("timed out after %s", r.opts.CaseTimeout)
	}
	return err
}

func oneLine(s string) string { return strings.ReplaceAll(s, "\n", `\n`) }

// state returns the running case or raises (helpers outside test.case).
func (r *testFile) state(L *lua.LState, what string) *caseState {
	if r.cur == nil {
		L.RaiseError("%s: only allowed inside test.case", what)
	}
	return r.cur
}

func (r *testFile) install(L *lua.LState) {
	t := L.NewTable()
	fns := map[string]lua.LGFunction{
		"case": func(L *lua.LState) int {
			name := L.CheckString(1)
			fn := L.CheckFunction(2)
			if r.cur != nil {
				L.RaiseError("test.case: cases cannot be nested")
			}
			r.cases = append(r.cases, testCase{name, fn})
			return 0
		},
		"fixture": r.luaFixture,
		"load":    r.luaLoad,
		"stub_exec": func(L *lua.LState) int {
			r.state(L, "test.stub_exec").backend.stub(L.CheckString(1), L.CheckString(2))
			return 0
		},
		"ssh_down": func(L *lua.LState) int {
			b := r.state(L, "test.ssh_down").backend
			b.mu.Lock()
			b.down[L.CheckString(1)] = true
			b.mu.Unlock()
			return 0
		},
		"write": func(L *lua.LState) int {
			r.fixtureWrite(L, "test.write", os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
			return 0
		},
		"append": func(L *lua.LState) int {
			r.fixtureWrite(L, "test.append", os.O_WRONLY|os.O_CREATE|os.O_APPEND)
			return 0
		},
		"remove": func(L *lua.LState) int {
			p := r.fixturePath(L, "test.remove")
			if err := os.Remove(p); err != nil {
				L.RaiseError("test.remove: %s", err.Error())
			}
			return 0
		},
		"exec_log": func(L *lua.LState) int {
			out := L.NewTable()
			for _, s := range r.state(L, "test.exec_log").backend.log() {
				out.Append(lua.LString(s))
			}
			L.Push(out)
			return 1
		},
		"eq": func(L *lua.LState) int {
			a, b := L.Get(1), L.Get(2)
			if !luaDeepEqual(a, b, 0) {
				raise(L, L.OptString(3, ""), fmt.Sprintf("expected %s, got %s", luaRepr(b), luaRepr(a)))
			}
			return 0
		},
		"ok": func(L *lua.LState) int {
			if !lua.LVAsBool(L.Get(1)) {
				raise(L, L.OptString(2, ""), "expected a true value, got "+luaRepr(L.Get(1)))
			}
			return 0
		},
		"contains": func(L *lua.LState) int {
			s, sub := L.CheckString(1), L.CheckString(2)
			if !strings.Contains(s, sub) {
				raise(L, L.OptString(3, ""), fmt.Sprintf("%q does not contain %q", s, sub))
			}
			return 0
		},
		"errors": func(L *lua.LState) int {
			fn := L.CheckFunction(1)
			sub := L.OptString(2, "")
			L.Push(fn)
			err := L.PCall(0, 0, nil)
			if err == nil {
				raise(L, L.OptString(3, ""), "expected an error")
				return 0
			}
			msg := err.Error()
			var ae *lua.ApiError
			if errors.As(err, &ae) && ae.Object != nil {
				msg = ae.Object.String()
			}
			if !strings.Contains(msg, sub) {
				raise(L, L.OptString(3, ""), fmt.Sprintf("error %q does not contain %q", msg, sub))
			}
			return 0
		},
		"fail": func(L *lua.LState) int {
			L.RaiseError("%s", L.OptString(1, "failed"))
			return 0
		},
	}
	for k, f := range fns {
		t.RawSetString(k, L.NewFunction(f))
	}
	L.SetGlobal("test", t)
}

func raise(L *lua.LState, msg, detail string) {
	if msg != "" {
		detail = msg + ": " + detail
	}
	L.RaiseError("%s", detail)
}

// luaFixture implements test.fixture{path = contents}: a fresh temp dir.
func (r *testFile) luaFixture(L *lua.LState) int {
	st := r.state(L, "test.fixture")
	files := L.OptTable(1, L.NewTable())
	type entry struct{ rel, body string }
	var entries []entry
	var bad error
	files.ForEach(func(k, v lua.LValue) {
		ks, ok1 := k.(lua.LString)
		vs, ok2 := v.(lua.LString)
		if bad != nil {
			return
		}
		if !ok1 || !ok2 {
			bad = errors.New("test.fixture: keys and values must be strings")
			return
		}
		rel := string(ks)
		clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
		if rel == "" || filepath.IsAbs(filepath.FromSlash(rel)) || strings.HasPrefix(rel, "/") || clean == ".." || strings.HasPrefix(clean, "../") || clean == "." {
			bad = fmt.Errorf("test.fixture: invalid path %q (must be relative, inside the fixture)", rel)
			return
		}
		entries = append(entries, entry{clean, string(vs)})
	})
	if bad != nil {
		L.RaiseError("%s", bad.Error())
		return 0
	}
	dir, err := os.MkdirTemp("", "mcplogview-fixture-")
	if err != nil {
		L.RaiseError("test.fixture: %s", err.Error())
		return 0
	}
	st.dirs = append(st.dirs, dir)
	for _, e := range entries {
		p := filepath.Join(dir, filepath.FromSlash(e.rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			L.RaiseError("test.fixture: %s", err.Error())
			return 0
		}
		if err := os.WriteFile(p, []byte(e.body), 0o644); err != nil {
			L.RaiseError("test.fixture: %s", err.Error())
			return 0
		}
	}
	L.Push(lua.LString(dir))
	return 1
}

// fixturePath checks that argument 1 is a path inside a fixture of the
// running case (test.fixture's result joined with a relative path).
func (r *testFile) fixturePath(L *lua.LState, what string) string {
	st := r.state(L, what)
	p := filepath.Clean(filepath.FromSlash(L.CheckString(1)))
	for _, d := range st.dirs[1:] { // dirs[0] is the backend's work dir
		if rel, err := filepath.Rel(d, p); err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			return p
		}
	}
	L.RaiseError("%s: %q is not inside a fixture of this case", what, L.CheckString(1))
	return ""
}

// fixtureWrite implements test.write/test.append(path, contents).
func (r *testFile) fixtureWrite(L *lua.LState, what string, flag int) {
	p := r.fixturePath(L, what)
	data := L.CheckString(2)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		L.RaiseError("%s: %s", what, err.Error())
	}
	f, err := os.OpenFile(p, flag, 0o644)
	if err != nil {
		L.RaiseError("%s: %s", what, err.Error())
	}
	_, err = f.WriteString(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		L.RaiseError("%s: %s", what, err.Error())
	}
}

// luaLoad implements test.load(cfg): the plugin under test in a fresh Host.
func (r *testFile) luaLoad(L *lua.LState) int {
	st := r.state(L, "test.load")
	ref := config.PluginRef{Name: filepath.Base(r.pluginDir)}
	if L.GetTop() >= 1 && L.Get(1) != lua.LNil {
		v, err := fromLua(L.CheckTable(1))
		if err != nil {
			L.RaiseError("test.load: %s", err.Error())
			return 0
		}
		m, _ := v.(map[string]any)
		if n, ok := m["name"].(string); ok {
			ref.Name = n
		}
		delete(m, "name")
		b, _ := json.Marshal(m)
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if len(m) > 0 {
			if err := dec.Decode(&ref); err != nil {
				L.RaiseError("test.load: bad config: %s", err.Error())
				return 0
			}
		}
	}
	h := NewHost(Options{SSH: st.backend, ReservedToolNames: r.opts.ReservedToolNames})
	st.hosts = append(st.hosts, h)
	h.Reload(config.Config{PluginDirs: []string{filepath.Dir(r.pluginDir)}, Plugins: []config.PluginRef{ref}})
	L.Push(r.handle(L, h, ref.Name))
	return 1
}

// handle builds the Lua object test.load returns. Methods take self first.
func (r *testFile) handle(L *lua.LState, h *Host, name string) *lua.LTable {
	t := L.NewTable()
	method := func(n string, f lua.LGFunction) { t.RawSetString(n, L.NewFunction(f)) }
	method("status", func(L *lua.LState) int {
		for _, s := range h.Status() {
			if s.Name == name {
				L.Push(lua.LBool(s.OK))
				if s.OK {
					L.Push(lua.LNil)
				} else {
					L.Push(lua.LString(s.Error))
				}
				return 2
			}
		}
		L.Push(lua.LFalse)
		L.Push(lua.LString("not loaded"))
		return 2
	})
	method("list", func(L *lua.LState) int {
		out := L.NewTable()
		row := func(id, label, desc, hint string, leaf bool) {
			e := L.NewTable()
			e.RawSetString("id", lua.LString(id))
			e.RawSetString("label", lua.LString(label))
			if desc != "" {
				e.RawSetString("description", lua.LString(desc))
			}
			if hint != "" {
				e.RawSetString("hint", lua.LString(hint))
			}
			e.RawSetString("leaf", lua.LBool(leaf))
			e.RawSetString("expandable", lua.LBool(!leaf))
			out.Append(e)
		}
		parent := L.OptString(2, "")
		if parent == "" {
			for _, rt := range h.Roots() {
				row(rt.ID, rt.Label, rt.Description, rt.Hint, false)
			}
		} else {
			kids, err := h.Children(callCtx(L), parent)
			if err != nil {
				L.RaiseError("%s", err.Error())
				return 0
			}
			for _, c := range kids {
				row(parent+"/"+c.Name, c.Label, c.Description, "", c.Leaf)
			}
		}
		L.Push(out)
		return 1
	})
	method("read", func(L *lua.LState) int {
		id := L.CheckString(2)
		// a cursor is the string a read returned, or a table, number or
		// boolean: the plugin's own cursor state (wrapped here like the core
		// does). A plugin's string state cannot be passed this way.
		cursor := ""
		switch cv := L.Get(3).(type) {
		case lua.LString:
			cursor = string(cv)
		case *lua.LTable, lua.LNumber, lua.LBool:
			st, err := fromLua(cv)
			if err == nil {
				cursor, err = wrapCursor(id, st)
			}
			if err != nil {
				L.RaiseError("h:read: bad cursor table: %s", err.Error())
				return 0
			}
		case *lua.LNilType:
		default:
			L.ArgError(3, "cursor must be a string (from a read), a table, a number, a boolean or nil")
			return 0
		}
		c, err := h.Read(callCtx(L), id, cursor, L.OptInt(4, defaultTailLines))
		if err != nil {
			L.RaiseError("%s", err.Error())
			return 0
		}
		lines := L.CreateTable(len(c.Lines), 0)
		for _, l := range c.Lines {
			lines.Append(lua.LString(l))
		}
		out := L.NewTable()
		out.RawSetString("lines", lines)
		out.RawSetString("cursor", lua.LString(c.Cursor))
		if st, err := unwrapCursor(id, c.Cursor); err == nil && st != nil {
			out.RawSetString("state", toLua(L, st))
		}
		out.RawSetString("eof", lua.LBool(c.EOF))
		L.Push(out)
		return 1
	})
	method("tool", func(L *lua.LState) int {
		tn := L.CheckString(2)
		var args map[string]any
		if L.Get(3) != lua.LNil {
			v, err := fromLua(L.CheckTable(3))
			if err != nil {
				L.RaiseError("%s", err.Error())
				return 0
			}
			switch a := v.(type) {
			case map[string]any:
				args = a
			case []any:
				if len(a) > 0 {
					L.RaiseError("h:tool: args must be a table with string keys")
					return 0
				}
			}
		}
		h.mu.RLock()
		var tl *tool
		for _, x := range h.tools {
			if x.def.Name == tn {
				tl = x
			}
		}
		h.mu.RUnlock()
		if tl == nil {
			L.RaiseError("unknown tool %q", tn)
			return 0
		}
		res, err := tl.call(callCtx(L), args)
		if err != nil {
			L.RaiseError("%s", err.Error())
			return 0
		}
		L.Push(toLua(L, res))
		return 1
	})
	method("skill", func(L *lua.LState) int {
		text, ok := h.Skill(L.CheckString(2))
		if !ok {
			L.Push(lua.LNil)
		} else {
			L.Push(lua.LString(text))
		}
		return 1
	})
	return t
}

// luaDeepEqual compares values, tables by content.
func luaDeepEqual(a, b lua.LValue, depth int) bool {
	ta, ok1 := a.(*lua.LTable)
	tb, ok2 := b.(*lua.LTable)
	if !ok1 || !ok2 {
		return a.Type() == b.Type() && lua.LVAsString(a) == lua.LVAsString(b) && (a.Type() != lua.LTFunction || a == b)
	}
	if ta == tb {
		return true
	}
	if depth > 100 {
		return false
	}
	eq := true
	n := 0
	ta.ForEach(func(k, v lua.LValue) {
		n++
		if eq && !luaDeepEqual(v, tb.RawGet(k), depth+1) {
			eq = false
		}
	})
	m := 0
	tb.ForEach(func(lua.LValue, lua.LValue) { m++ })
	return eq && n == m
}

// luaRepr renders a value for failure messages.
func luaRepr(v lua.LValue) string {
	switch v.Type() {
	case lua.LTString:
		return fmt.Sprintf("%q", string(v.(lua.LString)))
	case lua.LTTable:
		if x, err := fromLua(v); err == nil {
			if b, err := json.Marshal(x); err == nil {
				return string(b)
			}
		}
	}
	return v.String()
}
