package plugin

import (
	"reflect"
	"testing"

	lua "github.com/yuin/gopher-lua"
)

func TestRoundTripConversion(t *testing.T) {
	L := lua.NewState()
	defer L.Close()
	cases := []any{
		[]any{1.0, 2.0, 3.0},
		map[string]any{"a": 1.0, "b": map[string]any{"c": "x"}},
		[]any{},
		nil,
		true,
		"s",
	}
	for _, c := range cases {
		got, err := fromLua(toLua(L, c))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, c) {
			t.Errorf("got %#v want %#v", got, c)
		}
	}
	if got := toLua(L, []string{"a"}); got.Type() != lua.LTTable {
		t.Errorf("[]string not table")
	}
	if got := toLua(L, 7); got != lua.LNumber(7) {
		t.Errorf("int: %v", got)
	}
}

func TestFromLuaRejectsFunctionsAndCycles(t *testing.T) {
	L := lua.NewState()
	defer L.Close()
	if _, err := fromLua(L.NewFunction(func(*lua.LState) int { return 0 })); err == nil {
		t.Error("function accepted")
	}
	if _, err := fromLua(L.NewUserData()); err == nil {
		t.Error("userdata accepted")
	}
	cyc := L.NewTable()
	cyc.RawSetString("self", cyc)
	if _, err := fromLua(cyc); err == nil {
		t.Error("cycle accepted")
	}
	deep := L.NewTable()
	cur := deep
	for i := 0; i < 40; i++ {
		n := L.NewTable()
		cur.RawSetInt(1, n)
		cur = n
	}
	if _, err := fromLua(deep); err == nil {
		t.Error("deep accepted")
	}
}

func TestFromLuaRejectsDAGBlowup(t *testing.T) {
	L := lua.NewState()
	defer L.Close()
	cur := L.NewTable()
	for i := 0; i < 30; i++ {
		n := L.NewTable()
		n.RawSetInt(1, cur)
		n.RawSetInt(2, cur)
		cur = n
	}
	if _, err := fromLua(cur); err == nil {
		t.Error("DAG blowup accepted")
	}
}
