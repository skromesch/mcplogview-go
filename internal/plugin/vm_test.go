package plugin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	lua "github.com/yuin/gopher-lua"
)

func runSrc(t *testing.T, ctx context.Context, v *vm, src string) (lua.LValue, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "t.lua")
	if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return v.runFile(ctx, p)
}

func TestSandboxHasNoDangerousGlobals(t *testing.T) {
	v := newVM()
	res, err := runSrc(t, context.Background(), v,
		`return {os, io, package, debug, require, dofile, loadfile, load, loadstring, print, module, getfenv, setfenv, newproxy, _printregs, string.dump, string.format ~= nil, table.concat ~= nil, math.floor ~= nil}`)
	if err != nil {
		t.Fatal(err)
	}
	tbl := res.(*lua.LTable)
	for i := 1; i <= 16; i++ {
		if tbl.RawGetInt(i) != lua.LNil {
			t.Errorf("global #%d is not nil", i)
		}
	}
	for i := 17; i <= 19; i++ {
		if tbl.RawGetInt(i) != lua.LTrue {
			t.Errorf("capability #%d missing", i)
		}
	}
}

func TestCallTimeoutStopsInfiniteLoop(t *testing.T) {
	v := newVM()
	fn, err := runSrc(t, context.Background(), v, `return function() while true do end end`)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = v.call(ctx, fn)
	if err == nil {
		t.Fatal("expected error")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("took too long: %v", time.Since(start))
	}
}

func TestStringRepIsCapped(t *testing.T) {
	v := newVM()
	fn, err := runSrc(t, context.Background(), v, `return function() return string.rep("x", 1e9) end`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = v.call(context.Background(), fn); err == nil {
		t.Fatal("expected error")
	}
	fn, _ = runSrc(t, context.Background(), v, `return function() return #string.rep("ab", 3, ",") end`)
	out, err := v.call(context.Background(), fn)
	if err != nil || len(out) != 1 || out[0] != lua.LNumber(8) {
		t.Fatalf("got %v %v", out, err)
	}
}

func TestCallReturnsLuaErrorMessage(t *testing.T) {
	v := newVM()
	fn, err := runSrc(t, context.Background(), v, `return function() error("boom") end`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = v.call(context.Background(), fn)
	if err == nil || !strings.Contains(err.Error(), "boom") || strings.Contains(err.Error(), "stack traceback") {
		t.Fatalf("bad error: %v", err)
	}
}

func TestAllocationBombsAreRejected(t *testing.T) {
	v := newVM()
	for _, body := range []string{
		`return string.rep("xx", 2^62)`,
		`return ("x"):rep(1e9)`,
		`return string.format("%1000000000d", 1)`,
		`return string.format("%.1000000000f", 1)`,
	} {
		fn, err := runSrc(t, context.Background(), v, "return function() "+body+" end")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := v.call(context.Background(), fn); err == nil {
			t.Errorf("%s: expected error", body)
		}
	}
	fn, err := runSrc(t, context.Background(), v, `return function() return string.rep("", 2^62), string.format("%5.2f|%%|%s", 1.5, "a") end`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := v.call(context.Background(), fn)
	if err != nil || len(out) != 2 || out[0] != lua.LString("") || out[1] != lua.LString(" 1.50|%|a") {
		t.Fatalf("got %v %v", out, err)
	}
}

// A "#!" first line is skipped (as LoadFile did) without shifting line numbers.
func TestRunFileSkipsShebang(t *testing.T) {
	v := newVM()
	res, err := runSrc(t, context.Background(), v, "#!/usr/bin/env lua\nreturn 7\n")
	if err != nil || res != lua.LNumber(7) {
		t.Fatalf("got %v %v", res, err)
	}
	_, err = runSrc(t, context.Background(), v, "#!/usr/bin/env lua\nerror('x')\n")
	if err == nil || !strings.Contains(err.Error(), "t.lua:2:") {
		t.Fatalf("line numbers must survive the skipped shebang: %v", err)
	}
}
