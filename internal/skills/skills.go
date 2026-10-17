// Package skills builds the per-source usage skill returned by get_log_skill.
package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/skromesch/mcplogview-go/internal/config"
)

type Structured struct {
	Name            string   `json:"name,omitempty"`
	SourceID        string   `json:"sourceId"`
	Version         int      `json:"version"`
	Tags            []string `json:"tags"`
	Purpose         string   `json:"purpose"`
	QuickStart      []string `json:"quickStart"`
	Queries         []string `json:"queries"`
	Troubleshooting []string `json:"troubleshooting"`
}

type Payload struct {
	SourceID         string     `json:"sourceId"`
	Hint             string     `json:"hint,omitempty"`
	Origin           string     `json:"origin"`
	Warnings         []string   `json:"warnings"`
	Structured       Structured `json:"structured"`
	RenderedMarkdown string     `json:"renderedMarkdown"`
}

func commonGuidance(src config.Source) (purpose string, quickStart, queries, troubleshooting []string) {
	purpose = src.Description
	if purpose == "" {
		purpose = fmt.Sprintf("Read and follow the %s log source.", src.Label)
	}
	quickStart = []string{
		fmt.Sprintf("Call read_log_chunk for %s with maxLines=100", src.ID),
		fmt.Sprintf("Use follow_log for %s during reproductions", src.ID),
	}
	queries = []string{
		fmt.Sprintf(`read_log_chunk {"sourceId":"%s","maxLines":100}`, src.ID),
		fmt.Sprintf(`follow_log {"sourceId":"%s","timeoutMs":5000,"pollIntervalMs":200}`, src.ID),
	}
	troubleshooting = []string{
		"If no lines are returned, verify source path/ssh connectivity.",
		"If cursor handling is unclear, continue with returned cursor values only.",
	}
	return
}

func fallback(src config.Source, hint string, warnings []string) Payload {
	purpose, qs, queries, ts := commonGuidance(src)
	if warnings == nil {
		warnings = []string{}
	}
	return Payload{
		SourceID: src.ID,
		Hint:     hint,
		Origin:   "fallback",
		Warnings: warnings,
		Structured: Structured{
			SourceID:        src.ID,
			Version:         1,
			Tags:            []string{src.Type, "logs"},
			Purpose:         purpose,
			QuickStart:      qs,
			Queries:         queries,
			Troubleshooting: ts,
		},
		RenderedMarkdown: fmt.Sprintf("# %s\n\nUse this source to inspect runtime log lines and follow new events.", src.Label),
	}
}

// Load returns the skill for a source: the built-in self skill, the
// <skillsDir>/<sourceId>.md file, or a generated fallback.
func Load(src config.Source, cfg config.Config) Payload {
	if src.ID == cfg.Logging.SelfSourceID {
		return BuiltinSelf(src.ID)
	}
	return LoadFromFile(src, cfg.SkillsDir)
}

func LoadFromFile(src config.Source, skillsDir string) Payload {
	path := filepath.Join(skillsDir, src.ID+".md")
	raw, err := os.ReadFile(path)
	if err != nil {
		return fallback(src, src.Hint, []string{"skill file not found: " + path})
	}

	parsed := ParseMarkdown(string(raw))
	hint := parsed.Meta.Hint
	if hint == "" {
		hint = src.Hint
	}
	if parsed.Meta.SourceID != "" && parsed.Meta.SourceID != src.ID {
		warnings := append(append([]string{}, parsed.Warnings...),
			fmt.Sprintf("front matter sourceId mismatch: expected=%s got=%s", src.ID, parsed.Meta.SourceID))
		return fallback(src, hint, warnings)
	}

	purpose, qs, queries, ts := commonGuidance(src)
	return Payload{
		SourceID: src.ID,
		Hint:     hint,
		Origin:   "file",
		Warnings: parsed.Warnings,
		Structured: Structured{
			Name:            parsed.Meta.Name,
			SourceID:        src.ID,
			Version:         parsed.Meta.Version,
			Tags:            parsed.Meta.Tags,
			Purpose:         purpose,
			QuickStart:      qs,
			Queries:         queries,
			Troubleshooting: ts,
		},
		RenderedMarkdown: parsed.Body,
	}
}

// BuiltinSelf is the built-in skill for the server's own log.
func BuiltinSelf(sourceID string) Payload {
	return Payload{
		SourceID: sourceID,
		Hint:     "Use this source to debug mcplogview runtime and MCP transport issues.",
		Origin:   "builtin",
		Warnings: []string{},
		Structured: Structured{
			Name:     "mcplogview-self-debug",
			SourceID: sourceID,
			Version:  1,
			Tags:     []string{"mcplogview", "debug", "transport", "tools"},
			Purpose:  "Diagnose mcplogview startup, transport connectivity, and tool invocation failures.",
			QuickStart: []string{
				"Call read_log_chunk on mcplogview-self with maxLines=200 to inspect startup lines.",
				`If tools fail, search recent lines for "tool=<name> failed".`,
				"For HTTP mode, verify request/response logs around 4xx/5xx statuses.",
			},
			Queries: []string{
				`read_log_chunk {"sourceId":"mcplogview-self","maxLines":200}`,
				`follow_log {"sourceId":"mcplogview-self","timeoutMs":5000,"pollIntervalMs":200}`,
				"read_log_chunk with previous cursor to inspect contiguous failure windows",
			},
			Troubleshooting: []string{
				`Check for "network listen failed" (port in use) and change transport.port.`,
				`Check for "tool=<name> failed" and the associated error details.`,
				"Check for malformed HTTP request/JSON parse errors in transport logs.",
			},
		},
		RenderedMarkdown: strings.Join([]string{
			"# mcplogview self-log debugging",
			"",
			"Use this source to debug the server itself.",
			"",
			"## Recommended flow",
			"1. Read the latest startup lines.",
			"2. Inspect failing tool invocations (`tool=<name> failed`).",
			"3. Follow the log during reproduce attempts.",
			"",
			"## Common signatures",
			"- `network listen failed` (port already in use)",
			"- `http request json parse failed`",
			"- `tool=<name> failed`",
		}, "\n"),
	}
}
