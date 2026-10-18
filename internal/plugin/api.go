package plugin

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	lua "github.com/yuin/gopher-lua"
)

var rootIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// installAPI sets the global mcp table of the plugin's VM.
func (p *instance) installAPI() {
	L := p.vm.L
	mcp := L.NewTable()
	module := func(name string, fns map[string]lua.LGFunction) {
		t := L.NewTable()
		for k, f := range fns {
			t.RawSetString(k, L.NewFunction(f))
		}
		mcp.RawSetString(name, t)
	}
	module("source", map[string]lua.LGFunction{"register": p.luaRegisterSource})
	module("tool", map[string]lua.LGFunction{"register": p.luaRegisterTool})
	module("skill", map[string]lua.LGFunction{"register": p.luaRegisterSkill})
	module("log", map[string]lua.LGFunction{
		"info": func(L *lua.LState) int {
			p.log.Info("plugin " + p.name + ": " + L.CheckString(1))
			return 0
		},
		"error": func(L *lua.LState) int {
			p.log.Error("plugin "+p.name, errors.New(L.CheckString(1)))
			return 0
		},
	})
	module("json", map[string]lua.LGFunction{"encode": luaJSONEncode, "decode": luaJSONDecode})
	module("str", map[string]lua.LGFunction{"quote": luaQuote, "lines": luaLines})
	module("time", map[string]lua.LGFunction{"shift_days": luaShiftDays})
	mcp.RawSetString("on", L.NewFunction(p.luaOn))
	if p.ssh != nil {
		mcp.RawSetString("ssh", L.NewFunction(p.ssh.luaSSH))
	}
	if p.fs != nil {
		p.fs.install(L, mcp)
	}
	L.SetGlobal("mcp", mcp)
}

// luaRegisterSource implements mcp.source.register{id,label,description,hint,children,read}.
func (p *instance) luaRegisterSource(L *lua.LState) int {
	if !p.loading {
		L.RaiseError("mcp.source.register: only allowed while the plugin loads (top level or setup)")
		return 0
	}
	t := L.CheckTable(1)
	id := stringField(L, t, "id")
	if !rootIDPattern.MatchString(id) || len(id) > maxSegmentLen {
		L.RaiseError("mcp.source.register: invalid id %q (lowercase letters, digits, '_', '-'; at most %d bytes)", id, maxSegmentLen)
		return 0
	}
	if owner, ok := p.taken[id]; ok {
		L.RaiseError("mcp.source.register: id %q is already used by %s", id, owner)
		return 0
	}
	for _, s := range p.sources {
		if s.root.ID == id {
			L.RaiseError("mcp.source.register: id %q registered twice", id)
			return 0
		}
	}
	children, ok1 := t.RawGetString("children").(*lua.LFunction)
	read, ok2 := t.RawGetString("read").(*lua.LFunction)
	if !ok1 || !ok2 {
		L.RaiseError("mcp.source.register: children and read must be functions")
		return 0
	}
	label := stringField(L, t, "label")
	if label == "" {
		label = id
	}
	p.sources = append(p.sources, &source{
		root: Root{
			ID: id, Label: label, Plugin: p.name,
			Description: stringField(L, t, "description"),
			Hint:        stringField(L, t, "hint"),
		},
		p: p, children: children, read: read,
	})
	return 0
}

// luaRegisterSkill implements mcp.skill.register{source, text}: the usage
// skill of a root id this plugin registered. Staged like source registrations.
func (p *instance) luaRegisterSkill(L *lua.LState) int {
	if !p.loading {
		L.RaiseError("mcp.skill.register: only allowed while the plugin loads (top level or setup)")
		return 0
	}
	t := L.CheckTable(1)
	id := stringField(L, t, "source")
	text, ok := t.RawGetString("text").(lua.LString)
	if !ok {
		L.RaiseError("mcp.skill.register: text must be a string")
		return 0
	}
	own := false
	for _, s := range p.sources {
		own = own || s.root.ID == id
	}
	if !own {
		L.RaiseError("mcp.skill.register: source %q is not a root registered by this plugin (register the source first)", id)
		return 0
	}
	if _, dup := p.skills[id]; dup {
		L.RaiseError("mcp.skill.register: skill for %q registered twice", id)
		return 0
	}
	p.skills[id] = string(text)
	return 0
}

// stringField reads an optional string field ("" when nil).
func stringField(L *lua.LState, t *lua.LTable, key string) string {
	switch v := t.RawGetString(key).(type) {
	case *lua.LNilType:
		return ""
	case lua.LString:
		return string(v)
	}
	L.RaiseError("field %q must be a string", key)
	return ""
}

func luaJSONEncode(L *lua.LState) int {
	v, err := fromLua(L.CheckAny(1))
	if err != nil {
		L.RaiseError("mcp.json.encode: %s", err.Error())
		return 0
	}
	b, err := json.Marshal(v)
	if err != nil {
		L.RaiseError("mcp.json.encode: %s", err.Error())
		return 0
	}
	L.Push(lua.LString(b))
	return 1
}

func luaJSONDecode(L *lua.LState) int {
	var v any
	if err := json.Unmarshal([]byte(L.CheckString(1)), &v); err != nil {
		L.RaiseError("mcp.json.decode: %s", err.Error())
		return 0
	}
	L.Push(toLua(L, v))
	return 1
}

// shellQuote mirrors logs.shellQuote: a single-quoted POSIX shell word.
func shellQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

func luaQuote(L *lua.LState) int {
	L.Push(lua.LString(shellQuote(L.CheckString(1))))
	return 1
}

// luaLines splits on "\n"; a trailing newline does not add an empty line.
func luaLines(L *lua.LState) int {
	s := L.CheckString(1)
	t := L.NewTable()
	if s != "" {
		for _, line := range strings.Split(strings.TrimSuffix(s, "\n"), "\n") {
			t.Append(lua.LString(line))
		}
	}
	L.Push(t)
	return 1
}

// luaShiftDays implements mcp.time.shift_days("YYYY-MM-DD", n).
func luaShiftDays(L *lua.LState) int {
	d, err := time.Parse(time.DateOnly, L.CheckString(1))
	if err != nil {
		L.RaiseError("mcp.time.shift_days: invalid date %q (want YYYY-MM-DD)", L.CheckString(1))
		return 0
	}
	L.Push(lua.LString(d.AddDate(0, 0, L.CheckInt(2)).Format(time.DateOnly)))
	return 1
}
