package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skromesch/mcplogview-go/internal/config"
)

func TestParseMarkdown(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		p := ParseMarkdown("---\nname: api\nsourceId: prod\nversion: 3\nhint: look here\ntags: [a, \"b\", 'c']\n---\n# Body\n")
		if p.Meta.Name != "api" || p.Meta.SourceID != "prod" || p.Meta.Version != 3 || p.Meta.Hint != "look here" {
			t.Fatalf("%+v", p.Meta)
		}
		if strings.Join(p.Meta.Tags, ",") != "a,b,c" || p.Body != "# Body\n" || len(p.Warnings) != 0 {
			t.Fatalf("%+v", p)
		}
	})
	t.Run("no front matter", func(t *testing.T) {
		p := ParseMarkdown("just text")
		if p.Body != "just text" || p.Meta.Version != 1 || len(p.Warnings) != 1 {
			t.Fatalf("%+v", p)
		}
	})
	t.Run("unterminated", func(t *testing.T) {
		p := ParseMarkdown("---\nname: x\nno end")
		if len(p.Warnings) != 1 || p.Meta.Name != "" {
			t.Fatalf("%+v", p)
		}
	})
	t.Run("invalid values fall back to defaults", func(t *testing.T) {
		p := ParseMarkdown("---\nversion: 0\nname: x\n---\nbody")
		if p.Meta.Name != "" || p.Meta.Version != 1 || p.Body != "body" || len(p.Warnings) == 0 {
			t.Fatalf("%+v", p)
		}
	})
	t.Run("malformed line warns but continues", func(t *testing.T) {
		p := ParseMarkdown("---\nname: x\nbroken\n---\nb")
		if p.Meta.Name != "x" || len(p.Warnings) != 1 {
			t.Fatalf("%+v", p)
		}
	})
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.SkillsDir = dir
	src := config.Source{Type: "local", ID: "app", Label: "App", Hint: "cfg hint"}

	// no file -> fallback with a warning
	p := Load(src, cfg)
	if p.Origin != "fallback" || len(p.Warnings) != 1 || p.Hint != "cfg hint" {
		t.Fatalf("%+v", p)
	}

	// file -> parsed, file hint wins
	_ = os.WriteFile(filepath.Join(dir, "app.md"), []byte("---\nname: n\nhint: file hint\n---\nHello"), 0o644)
	p = Load(src, cfg)
	if p.Origin != "file" || p.Hint != "file hint" || p.RenderedMarkdown != "Hello" || p.Structured.Name != "n" {
		t.Fatalf("%+v", p)
	}

	// sourceId mismatch -> fallback + warning
	_ = os.WriteFile(filepath.Join(dir, "app.md"), []byte("---\nsourceId: other\n---\nHello"), 0o644)
	p = Load(src, cfg)
	if p.Origin != "fallback" || len(p.Warnings) != 1 {
		t.Fatalf("%+v", p)
	}

	// self source -> builtin
	self := config.Source{Type: "local", ID: cfg.Logging.SelfSourceID, Label: "self"}
	if p = Load(self, cfg); p.Origin != "builtin" || p.Structured.Name != "mcplogview-self-debug" {
		t.Fatalf("%+v", p)
	}
}
