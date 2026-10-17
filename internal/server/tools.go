// Package server wires the MCP tools and transports together.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/skromesch/mcplogview-go/internal/buildinfo"
	"github.com/skromesch/mcplogview-go/internal/config"
	"github.com/skromesch/mcplogview-go/internal/logs"
	"github.com/skromesch/mcplogview-go/internal/serverlog"
	"github.com/skromesch/mcplogview-go/internal/skills"
)

// Runtime is everything the tool handlers need. Sources and skills settings
// can be swapped at runtime (config hot reload) without restarting.
type Runtime struct {
	Logger serverlog.Logger
	state  atomic.Pointer[runtimeState]
}

type runtimeState struct {
	cfg config.Config
	reg *logs.Registry
}

// NewRuntime builds a Runtime serving the enabled sources of cfg.
func NewRuntime(cfg config.Config, logger serverlog.Logger) *Runtime {
	rt := &Runtime{Logger: logger}
	rt.Apply(cfg)
	return rt
}

// Apply swaps in a new configuration (only sources and skills are affected).
func (rt *Runtime) Apply(cfg config.Config) {
	rt.state.Store(&runtimeState{cfg: cfg, reg: logs.NewRegistry(cfg.EnabledSources())})
}

// Config returns the configuration currently in effect.
func (rt *Runtime) Config() config.Config { return rt.state.Load().cfg }

// Registry returns the currently enabled sources.
func (rt *Runtime) Registry() *logs.Registry { return rt.state.Load().reg }

type unknownSourceError struct{ id string }

func (e unknownSourceError) Error() string { return "unknown source: " + e.id }

type (
	listInput  struct{}
	skillInput struct {
		SourceID string `json:"sourceId" jsonschema:"id of a configured log source"`
	}
	readInput struct {
		SourceID string  `json:"sourceId" jsonschema:"id of a configured log source"`
		Cursor   *string `json:"cursor,omitempty" jsonschema:"cursor returned by a previous call; omit to start from the beginning"`
		MaxLines *int    `json:"maxLines,omitempty" jsonschema:"1..500, default 100"`
	}
	followInput struct {
		SourceID       string  `json:"sourceId" jsonschema:"id of a configured log source"`
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
	}
)

// ToolNames lists the registered tools in registration order.
var ToolNames = []string{"list_logs", "get_log_skill", "read_log_chunk", "follow_log"}

// New builds the MCP server with all tools registered.
func New(rt *Runtime) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "mcplogview", Version: buildinfo.ShortVersion()}, nil)

	mcp.AddTool(srv, &mcp.Tool{Name: "list_logs", Description: "List configured log sources"},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ listInput) (*mcp.CallToolResult, any, error) {
			return rt.run("list_logs", "", func() (any, error) {
				entries := []logEntry{}
				for _, s := range rt.Registry().List() {
					entries = append(entries, logEntry{ID: s.ID, Label: s.Label, Description: s.Description, Hint: s.Hint})
				}
				return map[string]any{"logs": entries}, nil
			})
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "get_log_skill", Description: "Get usage skill for a configured log source"},
		func(ctx context.Context, _ *mcp.CallToolRequest, in skillInput) (*mcp.CallToolResult, any, error) {
			return rt.run("get_log_skill", "sourceId="+in.SourceID, func() (any, error) {
				src, ok := rt.Registry().Get(in.SourceID)
				if !ok {
					return nil, unknownSourceError{in.SourceID}
				}
				return skills.Load(src, rt.Config()), nil
			})
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "read_log_chunk", Description: "Read a chunk of lines from a configured log source"},
		func(ctx context.Context, _ *mcp.CallToolRequest, in readInput) (*mcp.CallToolResult, any, error) {
			maxLines := intOr(in.MaxLines, 100)
			return rt.run("read_log_chunk", fmt.Sprintf("sourceId=%s maxLines=%d", in.SourceID, maxLines), func() (any, error) {
				if err := inRange("maxLines", maxLines, 1, 500); err != nil {
					return nil, err
				}
				src, ok := rt.Registry().Get(in.SourceID)
				if !ok {
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
			return rt.run("follow_log", ctxInfo, func() (any, error) {
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
					return nil, unknownSourceError{in.SourceID}
				}
				return follow(ctx, src, strOr(in.Cursor), maxLines, time.Duration(timeoutMs)*time.Millisecond, time.Duration(pollMs)*time.Millisecond)
			})
		})

	return srv
}

// follow polls until lines show up or the timeout / request context ends.
func follow(ctx context.Context, src config.Source, cursor string, maxLines int, timeout, poll time.Duration) (logs.Chunk, error) {
	deadline := time.Now().Add(timeout)
	for {
		res, err := logs.ReadChunk(src, cursor, maxLines)
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

// run logs the call and converts the result / error into an MCP tool result.
func (rt *Runtime) run(tool, callCtx string, fn func() (any, error)) (*mcp.CallToolResult, any, error) {
	rt.Logger.Info(trimSpace("tool=" + tool + " " + callCtx))
	value, err := fn()
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
