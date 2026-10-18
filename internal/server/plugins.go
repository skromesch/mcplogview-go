package server

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/skromesch/mcplogview-go/internal/config"
	"github.com/skromesch/mcplogview-go/internal/logs"
	"github.com/skromesch/mcplogview-go/internal/plugin"
	"github.com/skromesch/mcplogview-go/internal/skills"
)

// pluginSig is what decides whether plugins must be reloaded: their own
// settings and the configured source ids (which plugin root ids must not
// collide with). Toggling or relabeling a source does not change it.
type pluginSig struct {
	plugins   []config.PluginRef
	dirs      []string
	sourceIDs []string
}

func pluginSigOf(cfg config.Config) pluginSig {
	ids := make([]string, len(cfg.Sources))
	for i, s := range cfg.Sources {
		ids[i] = s.ID
	}
	slices.Sort(ids)
	return pluginSig{plugins: cfg.Plugins, dirs: cfg.PluginDirs, sourceIDs: ids}
}

// reloadPlugins reloads the plugin host when the plugin-relevant part of cfg
// changed since the previous call (always on the first).
func (rt *Runtime) reloadPlugins(cfg config.Config) {
	sig := pluginSigOf(cfg)
	if rt.sig != nil && reflect.DeepEqual(*rt.sig, sig) {
		return
	}
	rt.sig = &sig
	rt.plugins.Reload(cfg)
}

// Plugins returns the Lua plugin host.
func (rt *Runtime) Plugins() *plugin.Host { return rt.plugins }

// Close unloads the plugins; call it when the server stops.
func (rt *Runtime) Close() { rt.plugins.Close() }

// pluginEntries lists the plugin roots for list_logs without a parent.
func (rt *Runtime) pluginEntries() []logEntry {
	var out []logEntry
	for _, r := range rt.plugins.Roots() {
		out = append(out, logEntry{ID: r.ID, Label: r.Label, Description: r.Description, Hint: r.Hint, Expandable: true, Plugin: r.Plugin})
	}
	return out
}

// pluginChildren lists the entries below a plugin source for list_logs.
func (rt *Runtime) pluginChildren(ctx context.Context, parent string) ([]logEntry, error) {
	if _, configured := rt.Registry().Get(parent); configured || !rt.plugins.Owns(parent) {
		return nil, unknownSourceError{parent}
	}
	kids, err := rt.plugins.Children(ctx, parent)
	if err != nil {
		return nil, err
	}
	owner := rt.plugins.PluginOf(parent)
	out := make([]logEntry, 0, len(kids))
	for _, c := range kids {
		out = append(out, logEntry{ID: parent + "/" + c.Name, Label: c.Label, Description: c.Description, Leaf: c.Leaf, Expandable: !c.Leaf, Plugin: owner})
	}
	return out, nil
}

// pluginChunk is a read/follow result of a plugin source: the chunk plus the
// plugin it came from. Configured sources return a bare logs.Chunk.
type pluginChunk struct {
	logs.Chunk
	Plugin string `json:"plugin,omitempty"`
}

// markChunk labels a plugin source's chunk with its plugin.
func (rt *Runtime) markChunk(id string, c logs.Chunk) pluginChunk {
	return pluginChunk{Chunk: c, Plugin: rt.plugins.PluginOf(id)}
}

// pluginSource reports whether id is a plugin source (and not a configured one).
func (rt *Runtime) pluginSource(id string) bool {
	if _, configured := rt.Registry().Get(id); configured {
		return false
	}
	return rt.plugins.Owns(id)
}

// pluginSkill builds the skill of a plugin source; children use their root's.
func (rt *Runtime) pluginSkill(id string) skills.Payload {
	rootID, _, _ := strings.Cut(id, "/")
	label, hint := id, ""
	for _, r := range rt.plugins.Roots() {
		if r.ID == rootID {
			hint = r.Hint
			if id == rootID {
				label = r.Label
			}
		}
	}
	text, _ := rt.plugins.Skill(rootID)
	p := skills.FromText(id, label, text)
	p.Hint = hint
	return p
}

// followWith polls read until lines show up or the timeout / ctx ends.
func followWith(ctx context.Context, read func(cursor string) (logs.Chunk, error), cursor string, timeout, poll time.Duration) (logs.Chunk, error) {
	deadline := time.Now().Add(timeout)
	for {
		res, err := read(cursor)
		if err != nil {
			return logs.Chunk{}, err
		}
		if len(res.Lines) > 0 {
			return res, nil
		}
		cursor = res.Cursor
		if !time.Now().Before(deadline) {
			return logs.Chunk{Lines: []string{}, Cursor: cursor, EOF: true}, nil
		}
		select {
		case <-ctx.Done():
			return logs.Chunk{Lines: []string{}, Cursor: cursor, EOF: true}, nil
		case <-time.After(poll):
		}
	}
}

// parentCtx is the log context of a list_logs call.
func parentCtx(parent *string) string {
	if strOr(parent) == "" {
		return ""
	}
	return "parent=" + *parent
}

// toolSink publishes the tools plugins register on an MCP server.
type toolSink struct {
	rt  *Runtime
	srv *mcp.Server
}

func (s *toolSink) Add(def plugin.ToolDef, call func(ctx context.Context, args map[string]any) (any, error)) {
	s.srv.AddTool(&mcp.Tool{Name: def.Name, Description: def.Description, InputSchema: def.Schema},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			res, _, err := s.rt.run(ctx, def.Name, "plugin="+def.Plugin, func() (any, error) {
				var args map[string]any
				if raw := req.Params.Arguments; len(raw) > 0 {
					if err := json.Unmarshal(raw, &args); err != nil {
						return nil, fmt.Errorf("arguments must be a JSON object: %w", err)
					}
				}
				return call(ctx, args)
			})
			return res, err
		})
}

func (s *toolSink) Remove(name string) { s.srv.RemoveTools(name) }
