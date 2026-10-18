package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/google/jsonschema-go/jsonschema"
	lua "github.com/yuin/gopher-lua"
)

var toolNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// ToolDef describes an MCP tool registered by a plugin.
type ToolDef struct {
	Name, Description, Plugin string
	// Schema is the JSON schema of the arguments; always type "object".
	Schema map[string]any
}

// ToolSink receives the tools plugins register. The Host calls it serialized
// and never from inside a plugin call, but it must not call back into the Host.
type ToolSink interface {
	// Add publishes a tool. call validates args against the tool's schema and
	// runs the plugin handler; the result is JSON-serialisable.
	Add(def ToolDef, call func(ctx context.Context, args map[string]any) (any, error))
	Remove(name string)
}

// tool is a registered tool of a loaded plugin.
type tool struct {
	def      ToolDef
	p        *instance
	handler  *lua.LFunction
	resolved *jsonschema.Resolved
}

// call validates args and runs the handler under the plugin's call lock.
func (t *tool) call(ctx context.Context, args map[string]any) (any, error) {
	if args == nil {
		args = map[string]any{}
	}
	if err := t.resolved.Validate(args); err != nil {
		return nil, fmt.Errorf("invalid arguments for %s: %w", t.def.Name, err)
	}
	var out any
	err := t.p.do(ctx, "tool "+t.def.Name, func(ctx context.Context) error {
		L := t.p.vm.L
		res, err := t.p.vm.call(ctx, t.handler, toLua(L, args))
		if err != nil {
			return err
		}
		out, err = fromLua(first(res))
		return err
	})
	return out, err
}

// SetSink sets where tools are published and immediately adds the tools that
// are already registered. Tools added to a previous sink are not removed
// from it.
func (h *Host) SetSink(s ToolSink) {
	h.sinkMu.Lock()
	defer h.sinkMu.Unlock()
	h.sink = s
	h.mu.RLock()
	tools := h.tools
	h.mu.RUnlock()
	h.addToSink(tools)
}

// addToSink publishes tools to the sink; the caller holds sinkMu.
func (h *Host) addToSink(tools []*tool) {
	h.added = h.added[:0]
	if h.sink == nil {
		return
	}
	for _, t := range tools {
		h.sink.Add(t.def, t.call)
		h.added = append(h.added, t.def.Name)
	}
}

// luaRegisterTool implements mcp.tool.register{name, description, schema, handler}.
func (p *instance) luaRegisterTool(L *lua.LState) int {
	if !p.loading {
		L.RaiseError("mcp.tool.register: only allowed while the plugin loads (top level or setup)")
		return 0
	}
	t := L.CheckTable(1)
	name := stringField(L, t, "name")
	if !toolNamePattern.MatchString(name) {
		L.RaiseError("mcp.tool.register: invalid name %q (want %s)", name, toolNamePattern)
		return 0
	}
	if owner, ok := p.toolTaken[name]; ok {
		L.RaiseError("mcp.tool.register: name %q is already used by %s", name, owner)
		return 0
	}
	for _, o := range p.tools {
		if o.def.Name == name {
			L.RaiseError("mcp.tool.register: name %q registered twice", name)
			return 0
		}
	}
	handler, ok := t.RawGetString("handler").(*lua.LFunction)
	if !ok {
		L.RaiseError("mcp.tool.register: handler must be a function")
		return 0
	}
	schema, resolved, err := toolSchema(t.RawGetString("schema"))
	if err != nil {
		L.RaiseError("mcp.tool.register: %s", err.Error())
		return 0
	}
	p.tools = append(p.tools, &tool{
		def:     ToolDef{Name: name, Description: stringField(L, t, "description"), Plugin: p.name, Schema: schema},
		p:       p,
		handler: handler, resolved: resolved,
	})
	return 0
}

// toolSchema converts and checks a Lua schema value (nil: any object).
func toolSchema(v lua.LValue) (map[string]any, *jsonschema.Resolved, error) {
	schema := map[string]any{}
	if v != lua.LNil {
		tbl, ok := v.(*lua.LTable)
		if !ok {
			return nil, nil, fmt.Errorf("schema must be a table")
		}
		g, err := fromLua(tbl)
		if err != nil {
			return nil, nil, fmt.Errorf("schema: %w", err)
		}
		if schema, ok = g.(map[string]any); !ok {
			return nil, nil, fmt.Errorf(`schema must be a JSON object with type "object"`)
		}
	}
	if typ, ok := schema["type"]; !ok {
		schema["type"] = "object"
	} else if typ != "object" {
		return nil, nil, fmt.Errorf(`schema type must be "object" (got %v)`, typ)
	}
	b, err := json.Marshal(schema)
	if err != nil {
		return nil, nil, fmt.Errorf("schema: %w", err)
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, nil, fmt.Errorf("invalid schema: %w", err)
	}
	r, err := s.Resolve(nil)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid schema: %w", err)
	}
	return schema, r, nil
}
