// Package plugin hosts sandboxed Lua plugins.
package plugin

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	lua "github.com/yuin/gopher-lua"
)

// maxStringRep is the largest string string.rep may produce.
const maxStringRep = 16 << 20

// vm is a sandboxed Lua state: only base (reduced), table, string and math.
type vm struct {
	mu sync.Mutex
	L  *lua.LState
}

func newVM() *vm {
	L := lua.NewState(lua.Options{SkipOpenLibs: true})
	for _, lib := range []struct {
		name string
		fn   lua.LGFunction
	}{
		{lua.BaseLibName, lua.OpenBase},
		{lua.TabLibName, lua.OpenTable},
		{lua.StringLibName, lua.OpenString},
		{lua.MathLibName, lua.OpenMath},
	} {
		L.Push(L.NewFunction(lib.fn))
		L.Push(lua.LString(lib.name))
		L.Call(1, 0)
	}
	// Known, accepted gaps: plugins are admin-installed trusted code, and the
	// context timeout only interrupts pure-Lua loops. Native-side allocation
	// bombs such as `s = s..s` doubling or pathological string.find/gsub
	// patterns are NOT defended against (only string.rep and string.format
	// widths are capped).
	// print/_printregs would write to the process stdout, which carries the MCP stream.
	for _, name := range []string{
		"dofile", "loadfile", "load", "loadstring", "require", "module", "print",
		"getfenv", "setfenv", "newproxy", "_printregs",
	} {
		L.SetGlobal(name, lua.LNil)
	}
	str := L.GetGlobal("string").(*lua.LTable)
	str.RawSetString("dump", lua.LNil)
	str.RawSetString("rep", L.NewFunction(cappedStringRep))
	format := str.RawGetString("format")
	str.RawSetString("format", L.NewFunction(func(L *lua.LState) int {
		if err := checkFormatSpec(L.CheckString(1)); err != nil {
			L.RaiseError("%s", err.Error())
			return 0
		}
		n := L.GetTop()
		L.Push(format)
		for i := 1; i <= n; i++ {
			L.Push(L.Get(i))
		}
		L.Call(n, lua.MultRet)
		return L.GetTop() - n
	}))
	return &vm{L: L}
}

// cappedStringRep is string.rep with a result size limit.
func cappedStringRep(L *lua.LState) int {
	s := L.CheckString(1)
	n := L.CheckInt(2)
	sep := L.OptString(3, "")
	if n <= 0 {
		L.Push(lua.LString(""))
		return 1
	}
	unit := len(s) + len(sep)
	if unit == 0 {
		L.Push(lua.LString(""))
		return 1
	}
	if n > (maxStringRep+len(sep))/unit {
		L.RaiseError("string.rep: result too large")
		return 0
	}
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(sep)
		}
		b.WriteString(s)
	}
	L.Push(lua.LString(b.String()))
	return 1
}

// call invokes fn under ctx and returns its results. Lua errors come back
// without a stack traceback; Go panics become errors.
func (v *vm) call(ctx context.Context, fn lua.LValue, args ...lua.LValue) (res []lua.LValue, err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	L := v.L
	defer func() {
		if r := recover(); r != nil {
			res, err = nil, fmt.Errorf("lua: panic: %v", r)
		}
	}()
	L.SetContext(ctx)
	defer L.RemoveContext()
	base := L.GetTop()
	defer L.SetTop(base)
	L.Push(fn)
	for _, a := range args {
		L.Push(a)
	}
	if perr := L.PCall(len(args), lua.MultRet, nil); perr != nil {
		if ae, ok := perr.(*lua.ApiError); ok && ae.Object != nil {
			return nil, fmt.Errorf("%s", ae.Object.String())
		}
		return nil, perr
	}
	n := L.GetTop() - base
	res = make([]lua.LValue, n)
	for i := 0; i < n; i++ {
		res[i] = L.Get(base + 1 + i)
	}
	return res, nil
}

// runFile loads and runs a Lua file, returning its (first) return value.
// Error positions use the file's path.
func (v *vm) runFile(ctx context.Context, path string) (lua.LValue, error) {
	return v.runFileAs(ctx, path, path)
}

// runFileAs is runFile with chunk as the name shown in error positions.
func (v *vm) runFileAs(ctx context.Context, path, chunk string) (lua.LValue, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(src) > 0 && src[0] == '#' {
		// Like LoadFile: skip a "#!" line, keeping its newline so line
		// numbers stay right.
		if nl := bytes.IndexByte(src, '\n'); nl >= 0 {
			src = src[nl:]
		} else {
			src = nil
		}
	}
	v.mu.Lock()
	fn, err := v.L.Load(bytes.NewReader(src), chunk)
	v.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("%s", loadErrMessage(err))
	}
	out, err := v.call(ctx, fn)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return lua.LNil, nil
	}
	return out[0], nil
}

func loadErrMessage(err error) string {
	if ae, ok := err.(*lua.ApiError); ok && ae.Object != nil {
		return ae.Object.String()
	}
	return err.Error()
}

// maxFormatNumber bounds widths and precisions in string.format specs.
const maxFormatNumber = 10000

// checkFormatSpec rejects format specs with a huge width or precision.
func checkFormatSpec(f string) error {
	for i := 0; i < len(f); i++ {
		if f[i] != '%' {
			continue
		}
		i++
		num := 0
		for i < len(f) && (f[i] == '.' || f[i] == '-' || f[i] == '+' || f[i] == ' ' || f[i] == '#' || (f[i] >= '0' && f[i] <= '9')) {
			if f[i] >= '0' && f[i] <= '9' {
				num = num*10 + int(f[i]-'0')
				if num > maxFormatNumber {
					return fmt.Errorf("string.format: width or precision too large")
				}
			} else {
				num = 0
			}
			i++
		}
	}
	return nil
}
