package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The example configs shown in the documentation must stay valid.
func TestDocumentationExamplesLoad(t *testing.T) {
	files, err := filepath.Glob("../../docs/examples/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no example configs found: %v", err)
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			if _, err := Load(f); err != nil {
				t.Fatalf("%s no longer loads: %v", f, err)
			}
		})
	}
}

// Every relative link and image in the documentation must point at a file that exists.
func TestDocumentationLinksResolve(t *testing.T) {
	re := regexp.MustCompile(`\]\(([^)\s]+)\)`)
	files, _ := filepath.Glob("../../docs/*.md")
	files = append(files, "../../README.md", "../../docs/screenshots/README.md")
	checked := 0
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(data), -1) {
			link := m[1]
			if strings.HasPrefix(link, "http") || strings.HasPrefix(link, "#") || strings.HasPrefix(link, "mailto:") {
				continue
			}
			path, anchor, _ := strings.Cut(link, "#")
			target := filepath.Join(filepath.Dir(f), path)
			if _, err := os.Stat(target); err != nil {
				t.Errorf("%s: broken link %q", f, link)
			}
			if anchor != "" && strings.HasSuffix(path, ".md") {
				body, _ := os.ReadFile(target)
				if !hasHeading(string(body), anchor) {
					t.Errorf("%s: link %q: no heading for anchor #%s in %s", f, link, anchor, path)
				}
			}
			checked++
		}
	}
	if checked < 30 {
		t.Fatalf("expected many links, only checked %d", checked)
	}
}

// hasHeading reports whether a markdown heading slugifies (GitHub style) to anchor.
func hasHeading(md, anchor string) bool {
	for _, line := range strings.Split(md, "\n") {
		if !strings.HasPrefix(line, "#") {
			continue
		}
		h := strings.ToLower(strings.TrimSpace(strings.TrimLeft(line, "#")))
		var b strings.Builder
		for _, r := range h {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
				b.WriteRune(r)
			case r == ' ':
				b.WriteRune('-')
			}
		}
		if b.String() == anchor {
			return true
		}
	}
	return false
}
