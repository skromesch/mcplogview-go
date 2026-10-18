package plugin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	lua "github.com/yuin/gopher-lua"

	"github.com/skromesch/mcplogview-go/internal/logs"
)

const (
	maxSegments   = 8
	maxSegmentLen = 128
	maxReadLines  = 500
)

// Root is a source tree root registered by a plugin.
type Root struct {
	ID, Label, Description, Hint, Plugin string
}

// Child is one entry below a plugin source path.
type Child struct {
	Name, Label, Description string
	Leaf                     bool
}

type source struct {
	root     Root
	p        *instance
	children *lua.LFunction
	read     *lua.LFunction
}

// Roots lists the registered roots in load order.
func (h *Host) Roots() []Root {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]Root, 0, len(h.order))
	for _, s := range h.order {
		out = append(out, s.root)
	}
	return out
}

// Owns reports whether the part of sourceID before the first "/" is a
// registered root.
func (h *Host) Owns(sourceID string) bool {
	root, _, _ := strings.Cut(sourceID, "/")
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.roots[root]
	return ok
}

// PluginOf returns the name of the plugin owning sourceID's root, or "".
func (h *Host) PluginOf(sourceID string) string {
	root, _, _ := strings.Cut(sourceID, "/")
	h.mu.RLock()
	defer h.mu.RUnlock()
	if s, ok := h.roots[root]; ok {
		return s.root.Plugin
	}
	return ""
}

// Children lists the entries below sourceID.
func (h *Host) Children(ctx context.Context, sourceID string) ([]Child, error) {
	s, path, err := h.lookup(sourceID)
	if err != nil {
		return nil, err
	}
	var out []Child
	err = s.p.do(ctx, "children", func(ctx context.Context) error {
		res, err := s.p.vm.call(ctx, s.children, pathTable(s.p.vm.L, path))
		if err != nil {
			return err
		}
		out, err = parseChildren(first(res))
		return err
	})
	return out, err
}

// Read returns up to maxLines lines of sourceID, continuing from cursor.
func (h *Host) Read(ctx context.Context, sourceID, cursor string, maxLines int) (logs.Chunk, error) {
	if maxLines < 1 || maxLines > maxReadLines {
		return logs.Chunk{}, fmt.Errorf("maxLines must be 1..%d (got %d)", maxReadLines, maxLines)
	}
	s, path, err := h.lookup(sourceID)
	if err != nil {
		return logs.Chunk{}, err
	}
	var state any
	if cursor != "" {
		if state, err = unwrapCursor(sourceID, cursor); err != nil {
			return logs.Chunk{}, err
		}
	}
	var chunk logs.Chunk
	err = s.p.do(ctx, "read", func(ctx context.Context) error {
		L := s.p.vm.L
		res, err := s.p.vm.call(ctx, s.read, pathTable(L, path), toLua(L, state), lua.LNumber(maxLines))
		if err != nil {
			return err
		}
		chunk, err = parseChunk(sourceID, first(res), maxLines)
		return err
	})
	return chunk, err
}

// lookup splits and validates sourceID and finds its root.
func (h *Host) lookup(sourceID string) (*source, []string, error) {
	segs := strings.Split(sourceID, "/")
	if len(segs) > maxSegments {
		return nil, nil, fmt.Errorf("invalid source id %q: more than %d segments", sourceID, maxSegments)
	}
	for _, seg := range segs {
		if err := checkSegment(seg); err != nil {
			return nil, nil, fmt.Errorf("invalid source id %q: %w", sourceID, err)
		}
	}
	h.mu.RLock()
	s := h.roots[segs[0]]
	h.mu.RUnlock()
	if s == nil {
		return nil, nil, fmt.Errorf("unknown plugin source: %s", sourceID)
	}
	return s, segs[1:], nil
}

func checkSegment(seg string) error {
	switch {
	case seg == "":
		return errors.New("empty segment")
	case len(seg) > maxSegmentLen:
		return fmt.Errorf("segment longer than %d bytes", maxSegmentLen)
	case strings.Contains(seg, "/"):
		return errors.New("segment contains '/'")
	case !utf8.ValidString(seg):
		return errors.New("segment is not valid UTF-8")
	case strings.IndexFunc(seg, unicode.IsControl) >= 0:
		return errors.New("segment contains a control character")
	}
	return nil
}

func pathTable(L *lua.LState, path []string) *lua.LTable {
	t := L.CreateTable(len(path), 0)
	for _, seg := range path {
		t.Append(lua.LString(seg))
	}
	return t
}

func first(res []lua.LValue) lua.LValue {
	if len(res) == 0 {
		return lua.LNil
	}
	return res[0]
}

// parseChildren validates children(path)'s result: an array of
// {name, label?, description?, leaf?}.
func parseChildren(v lua.LValue) ([]Child, error) {
	t, ok := v.(*lua.LTable)
	if !ok {
		return nil, fmt.Errorf("children must return a table, got %s", v.Type())
	}
	out := make([]Child, 0, t.Len())
	for i := 1; i <= t.Len(); i++ {
		e, ok := t.RawGetInt(i).(*lua.LTable)
		if !ok {
			return nil, fmt.Errorf("children[%d] must be a table", i)
		}
		var c Child
		var err error
		if c.Name, err = luaString(e, "name"); err != nil {
			return nil, fmt.Errorf("children[%d]: %w", i, err)
		}
		if err := checkSegment(c.Name); err != nil {
			return nil, fmt.Errorf("children[%d]: invalid name %q: %w", i, c.Name, err)
		}
		if c.Label, err = luaString(e, "label"); err != nil {
			return nil, fmt.Errorf("children[%d]: %w", i, err)
		}
		if c.Label == "" {
			c.Label = c.Name
		}
		if c.Description, err = luaString(e, "description"); err != nil {
			return nil, fmt.Errorf("children[%d]: %w", i, err)
		}
		if c.Leaf, err = luaBool(e, "leaf"); err != nil {
			return nil, fmt.Errorf("children[%d]: %w", i, err)
		}
		out = append(out, c)
	}
	return out, nil
}

// parseChunk validates read()'s result {lines, cursor?, eof?} and wraps the
// cursor state for sourceID.
func parseChunk(sourceID string, v lua.LValue, maxLines int) (logs.Chunk, error) {
	t, ok := v.(*lua.LTable)
	if !ok {
		return logs.Chunk{}, fmt.Errorf("read must return a table, got %s", v.Type())
	}
	lines := make([]string, 0)
	switch lt := t.RawGetString("lines").(type) {
	case *lua.LNilType:
	case *lua.LTable:
		n := lt.Len()
		if n > maxLines {
			return logs.Chunk{}, fmt.Errorf("read returned %d lines, max %d", n, maxLines)
		}
		for i := 1; i <= n; i++ {
			s, ok := lt.RawGetInt(i).(lua.LString)
			if !ok {
				return logs.Chunk{}, fmt.Errorf("lines[%d] must be a string", i)
			}
			lines = append(lines, string(s))
		}
	default:
		return logs.Chunk{}, errors.New("lines must be a table")
	}
	eof, err := luaBool(t, "eof")
	if err != nil {
		return logs.Chunk{}, err
	}
	state, err := fromLua(t.RawGetString("cursor"))
	if err != nil {
		return logs.Chunk{}, fmt.Errorf("invalid cursor: %w", err)
	}
	cursor, err := wrapCursor(sourceID, state)
	if err != nil {
		return logs.Chunk{}, fmt.Errorf("invalid cursor: %w", err)
	}
	return logs.Chunk{Lines: lines, Cursor: cursor, EOF: eof}, nil
}

func luaString(t *lua.LTable, key string) (string, error) {
	switch v := t.RawGetString(key).(type) {
	case *lua.LNilType:
		return "", nil
	case lua.LString:
		return string(v), nil
	}
	return "", fmt.Errorf("%s must be a string", key)
}

func luaBool(t *lua.LTable, key string) (bool, error) {
	switch v := t.RawGetString(key).(type) {
	case *lua.LNilType:
		return false, nil
	case lua.LBool:
		return bool(v), nil
	}
	return false, fmt.Errorf("%s must be a boolean", key)
}

// wrappedCursor is the cursor the host hands out: the plugin's state tagged
// with the full source id, as base64 JSON (like logs' cursors).
type wrappedCursor struct {
	S string `json:"s"`
	C any    `json:"c"`
}

func wrapCursor(sourceID string, state any) (string, error) {
	b, err := json.Marshal(wrappedCursor{S: sourceID, C: state})
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

func unwrapCursor(sourceID, cursor string) (any, error) {
	b, err := base64.StdEncoding.DecodeString(cursor)
	if err != nil {
		return nil, fmt.Errorf("invalid cursor: %w", err)
	}
	var w wrappedCursor
	if err := json.Unmarshal(b, &w); err != nil {
		return nil, fmt.Errorf("invalid cursor: %w", err)
	}
	if w.S == "" {
		return nil, fmt.Errorf("cursor does not belong to source %q (it was issued by another source)", sourceID)
	}
	if w.S != sourceID {
		return nil, fmt.Errorf("cursor belongs to source %q, not %q", w.S, sourceID)
	}
	return w.C, nil
}
