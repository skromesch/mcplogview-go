// Package server wires the MCP tools and transports together.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/skromesch/mcplogview-go/internal/buildinfo"
	"github.com/skromesch/mcplogview-go/internal/config"
	"github.com/skromesch/mcplogview-go/internal/logs"
	"github.com/skromesch/mcplogview-go/internal/plugin"
	"github.com/skromesch/mcplogview-go/internal/serverlog"
	"github.com/skromesch/mcplogview-go/internal/skills"
)

// Runtime is everything the tool handlers need. Sources and skills settings
// can be swapped at runtime (config hot reload) without restarting.
type Runtime struct {
	Logger  serverlog.Logger
	state   atomic.Pointer[runtimeState]
	plugins *plugin.Host

	applyMu sync.Mutex
	sig     *pluginSig // plugin settings of the previous Apply
}

type runtimeState struct {
	cfg config.Config
	reg *logs.Registry
}

// NewRuntime builds a Runtime serving the enabled sources of cfg.
func NewRuntime(cfg config.Config, logger serverlog.Logger) *Runtime {
	rt := &Runtime{Logger: logger, plugins: plugin.NewHost(plugin.Options{Logger: logger, ReservedToolNames: ToolNames})}
	rt.Apply(cfg)
	return rt
}

// Apply swaps in a new configuration (sources, skills and plugins are
// affected). Plugins are reloaded only when their settings or the set of
// configured source ids changed.
func (rt *Runtime) Apply(cfg config.Config) {
	rt.applyMu.Lock()
	defer rt.applyMu.Unlock()
	rt.reloadPlugins(cfg)
	rt.state.Store(&runtimeState{cfg: cfg, reg: logs.NewRegistry(cfg.EnabledSources())})
}

// Config returns the configuration currently in effect.
func (rt *Runtime) Config() config.Config { return rt.state.Load().cfg }

// Registry returns the currently enabled sources.
func (rt *Runtime) Registry() *logs.Registry { return rt.state.Load().reg }

type unknownSourceError struct{ id string }

func (e unknownSourceError) Error() string { return "unknown source: " + e.id }

type (
	listInput struct {
		Parent *string `json:"parent,omitempty" jsonschema:"id of an expandable plugin source whose children to list; omit for the top level"`
	}
	skillInput struct {
		SourceID string `json:"sourceId" jsonschema:"a log source id from list_logs; plugin sources use ids like root/part/part found via list_logs parent=<id>"`
	}
	readInput struct {
		SourceID string  `json:"sourceId" jsonschema:"a log source id from list_logs; plugin sources use ids like root/part/part found via list_logs parent=<id>"`
		Cursor   *string `json:"cursor,omitempty" jsonschema:"cursor returned by a previous call; omit to start from the beginning (a plugin source may start elsewhere, see its skill)"`
		MaxLines *int    `json:"maxLines,omitempty" jsonschema:"1..500, default 100"`
	}
	followInput struct {
		SourceID       string  `json:"sourceId" jsonschema:"a log source id from list_logs; plugin sources use ids like root/part/part found via list_logs parent=<id>"`
		Cursor         *string `json:"cursor,omitempty" jsonschema:"cursor returned by a previous call"`
		MaxLines       *int    `json:"maxLines,omitempty" jsonschema:"1..500, default 100"`
		TimeoutMs      *int    `json:"timeoutMs,omitempty" jsonschema:"1..30000, default 5000"`
		PollIntervalMs *int    `json:"pollIntervalMs,omitempty" jsonschema:"10..1000, default 200"`
	}
	logEntry struct {
		ID          string `json:"id"`
		Label       string `json:"label"`
		Description string `json:"description,omitempty"`
		Hint        string `json:"hint,omitempty"`
		Leaf        bool   `json:"leaf,omitempty"`
		Expandable  bool   `json:"expandable,omitempty"`
		// Plugin names the plugin a source comes from (plugin sources only).
		Plugin string `json:"plugin,omitempty"`
	}
)

// ToolNames lists the registered tools in registration order.
var ToolNames = []string{"list_logs", "get_log_skill", "read_log_chunk", "follow_log"}

// New builds the MCP server with all tools registered.
func New(rt *Runtime) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "mcplogview", Version: buildinfo.ShortVersion()}, nil)

	mcp.AddTool(srv, &mcp.Tool{Name: "list_logs", Description: "List log sources: configured ones and plugin sources (pass parent=<id> to list an expandable plugin source's children)"},
		func(ctx context.Context, _ *mcp.CallToolRequest, in listInput) (*mcp.CallToolResult, any, error) {
			return rt.run(ctx, "list_logs", parentCtx(in.Parent), func() (any, error) {
				if parent := strOr(in.Parent); parent != "" {
					entries, err := rt.pluginChildren(ctx, parent)
					if err != nil {
						return nil, err
					}
					return map[string]any{"logs": entries}, nil
				}
				entries := []logEntry{}
				for _, s := range rt.Registry().List() {
					entries = append(entries, logEntry{ID: s.ID, Label: s.Label, Description: s.Description, Hint: s.Hint})
				}
				entries = append(entries, rt.pluginEntries()...)
				return map[string]any{"logs": entries}, nil
			})
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "get_log_skill", Description: "Get usage skill for a log source (configured or plugin)"},
		func(ctx context.Context, _ *mcp.CallToolRequest, in skillInput) (*mcp.CallToolResult, any, error) {
			return rt.run(ctx, "get_log_skill", "sourceId="+in.SourceID, func() (any, error) {
				src, ok := rt.Registry().Get(in.SourceID)
				if !ok {
					if rt.pluginSource(in.SourceID) {
						return rt.pluginSkill(in.SourceID), nil
					}
					return nil, unknownSourceError{in.SourceID}
				}
				return skills.Load(src, rt.Config()), nil
			})
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "read_log_chunk", Description: "Read a chunk of lines from a log source (configured or plugin)"},
		func(ctx context.Context, _ *mcp.CallToolRequest, in readInput) (*mcp.CallToolResult, any, error) {
			maxLines := intOr(in.MaxLines, 100)
			return rt.run(ctx, "read_log_chunk", fmt.Sprintf("sourceId=%s maxLines=%d", in.SourceID, maxLines), func() (any, error) {
				if err := inRange("maxLines", maxLines, 1, 500); err != nil {
					return nil, err
				}
				src, ok := rt.Registry().Get(in.SourceID)
				if !ok {
					if rt.pluginSource(in.SourceID) {
						c, err := rt.plugins.Read(ctx, in.SourceID, strOr(in.Cursor), maxLines)
						return rt.markChunk(in.SourceID, c), err
					}
					return nil, unknownSourceError{in.SourceID}
				}
				return logs.ReadChunk(src, strOr(in.Cursor), maxLines)
			})
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "follow_log", Description: "Wait briefly for newly appended log lines"},
		func(ctx context.Context, _ *mcp.CallToolRequest, in followInput) (*mcp.CallToolResult, any, error) {
			maxLines := intOr(in.MaxLines, 100)
			timeoutMs := intOr(in.TimeoutMs, 5000)
			pollMs := intOr(in.PollIntervalMs, 200)
			ctxInfo := fmt.Sprintf("sourceId=%s maxLines=%d timeoutMs=%d pollIntervalMs=%d", in.SourceID, maxLines, timeoutMs, pollMs)
			return rt.run(ctx, "follow_log", ctxInfo, func() (any, error) {
				for _, c := range []struct {
					name      string
					v, lo, hi int
				}{{"maxLines", maxLines, 1, 500}, {"timeoutMs", timeoutMs, 1, 30000}, {"pollIntervalMs", pollMs, 10, 1000}} {
					if err := inRange(c.name, c.v, c.lo, c.hi); err != nil {
						return nil, err
					}
				}
				src, ok := rt.Registry().Get(in.SourceID)
				if !ok {
					if rt.pluginSource(in.SourceID) {
						read := func(cursor string) (logs.Chunk, error) {
							return rt.plugins.Read(ctx, in.SourceID, cursor, maxLines)
						}
						c, err := followWith(ctx, read, strOr(in.Cursor), time.Duration(timeoutMs)*time.Millisecond, time.Duration(pollMs)*time.Millisecond)
						return rt.markChunk(in.SourceID, c), err
					}
					return nil, unknownSourceError{in.SourceID}
				}
				return follow(ctx, src, strOr(in.Cursor), maxLines, time.Duration(timeoutMs)*time.Millisecond, time.Duration(pollMs)*time.Millisecond)
			})
		})

	rt.plugins.SetSink(&toolSink{rt: rt, srv: srv})
	return srv
}

// follow polls a configured source until lines show up or the timeout /
// request context ends.
func follow(ctx context.Context, src config.Source, cursor string, maxLines int, timeout, poll time.Duration) (logs.Chunk, error) {
	return followWith(ctx, func(c string) (logs.Chunk, error) { return logs.ReadChunk(src, c, maxLines) }, cursor, timeout, poll)
}

// run logs the call and converts the result / error into an MCP tool result.
func (rt *Runtime) run(ctx context.Context, tool, callCtx string, fn func() (any, error)) (*mcp.CallToolResult, any, error) {
	rt.Logger.Info(trimSpace("tool=" + tool + " " + callCtx))
	if allow, name, reason := rt.plugins.CheckTool(ctx, tool, callCtx); !allow {
		rt.Logger.Info(trimSpace("tool=" + tool + " denied by plugin " + name + " " + callCtx))
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Tool denied by plugin %s: %s", name, reason)}},
		}, nil, nil
	}
	value, err := fn()
	// after_tool runs in the background (it must not delay the response) and
	// detached from ctx (a cancelled request still reports). err is the
	// call's or the marshalling's error, read when this returns.
	defer func() { rt.plugins.AfterToolAsync(ctx, tool, callCtx, err) }()
	if err != nil {
		rt.Logger.Error(trimSpace("tool="+tool+" failed "+callCtx), err)
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Tool error (%s): %s", tool, err)}},
		}, nil, nil
	}
	payload, err := json.Marshal(value)
	if err != nil {
		rt.Logger.Error("tool="+tool+" marshal failed", err)
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Tool error (%s): %s", tool, err)}},
		}, nil, nil
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(payload)}}}, nil, nil
}

func inRange(name string, v, lo, hi int) error {
	if v < lo || v > hi {
		return fmt.Errorf("%s must be between %d and %d (got %d)", name, lo, hi, v)
	}
	return nil
}

func intOr(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

func strOr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func trimSpace(s string) string {
	for len(s) > 0 && s[len(s)-1] == ' ' {
		s = s[:len(s)-1]
	}
	return s
}
