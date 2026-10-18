package plugin

import (
	"errors"
	"fmt"
	"reflect"

	lua "github.com/yuin/gopher-lua"
)

const maxConvertDepth = 32

// toLua converts a Go value (JSON-like data) into a Lua value.
func toLua(L *lua.LState, v any) lua.LValue {
	switch x := v.(type) {
	case nil:
		return lua.LNil
	case lua.LValue:
		return x
	case bool:
		return lua.LBool(x)
	case string:
		return lua.LString(x)
	case float64:
		return lua.LNumber(x)
	case int:
		return lua.LNumber(x)
	case int64:
		return lua.LNumber(x)
	case []any:
		t := L.CreateTable(len(x), 0)
		for _, e := range x {
			t.Append(toLua(L, e))
		}
		return t
	case map[string]any:
		t := L.CreateTable(0, len(x))
		for k, e := range x {
			t.RawSetString(k, toLua(L, e))
		}
		return t
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return lua.LNumber(rv.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return lua.LNumber(rv.Uint())
	case reflect.Float32:
		return lua.LNumber(rv.Float())
	case reflect.String:
		return lua.LString(rv.String())
	case reflect.Slice, reflect.Array:
		t := L.CreateTable(rv.Len(), 0)
		for i := 0; i < rv.Len(); i++ {
			t.Append(toLua(L, rv.Index(i).Interface()))
		}
		return t
	case reflect.Map:
		if rv.Type().Key().Kind() == reflect.String {
			t := L.CreateTable(0, rv.Len())
			for it := rv.MapRange(); it.Next(); {
				t.RawSetString(it.Key().String(), toLua(L, it.Value().Interface()))
			}
			return t
		}
	}
	return lua.LString(fmt.Sprint(v))
}

// fromLua converts a Lua value into JSON-like Go data: array tables become
// []any, other tables map[string]any, numbers float64. Functions, userdata,
// cycles and nesting deeper than 32 levels are errors.
func fromLua(v lua.LValue) (any, error) {
	return fromLuaDepth(v, 0, map[*lua.LTable]bool{}, new(int))
}

// maxConvertNodes bounds the total values visited, so shared sub-tables
// (a DAG) cannot expand exponentially.
const maxConvertNodes = 100000

func fromLuaDepth(v lua.LValue, depth int, seen map[*lua.LTable]bool, nodes *int) (any, error) {
	if *nodes++; *nodes > maxConvertNodes {
		return nil, errors.New("lua value too large")
	}
	switch x := v.(type) {
	case *lua.LNilType:
		return nil, nil
	case lua.LBool:
		return bool(x), nil
	case lua.LNumber:
		return float64(x), nil
	case lua.LString:
		return string(x), nil
	case *lua.LTable:
		if depth >= maxConvertDepth {
			return nil, errors.New("lua value nested too deeply")
		}
		if seen[x] {
			return nil, errors.New("lua value contains a cycle")
		}
		seen[x] = true
		defer delete(seen, x)
		return tableFromLua(x, depth, seen, nodes)
	}
	return nil, fmt.Errorf("cannot convert lua %s to a Go value", v.Type())
}

func tableFromLua(t *lua.LTable, depth int, seen map[*lua.LTable]bool, nodes *int) (any, error) {
	count := 0
	isArray := true
	t.ForEach(func(k, _ lua.LValue) {
		count++
		if n, ok := k.(lua.LNumber); !ok || float64(n) != float64(int(n)) || n < 1 || int(n) > t.Len() {
			isArray = false
		}
	})
	if isArray && count == t.Len() {
		out := make([]any, 0, count)
		for i := 1; i <= count; i++ {
			e, err := fromLuaDepth(t.RawGetInt(i), depth+1, seen, nodes)
			if err != nil {
				return nil, err
			}
			out = append(out, e)
		}
		return out, nil
	}
	out := make(map[string]any, count)
	var ferr error
	t.ForEach(func(k, e lua.LValue) {
		if ferr != nil {
			return
		}
		var key string
		switch kk := k.(type) {
		case lua.LString:
			key = string(kk)
		case lua.LNumber:
			key = kk.String()
		default:
			ferr = fmt.Errorf("unsupported table key type %s", k.Type())
			return
		}
		val, err := fromLuaDepth(e, depth+1, seen, nodes)
		if err != nil {
			ferr = err
			return
		}
		out[key] = val
	})
	if ferr != nil {
		return nil, ferr
	}
	return out, nil
}
