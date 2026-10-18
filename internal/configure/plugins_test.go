package configure

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetPluginEnabledKeepsTheRestOfTheFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	orig := `{"custom": 1, "sources": [], "plugins": [
		{"name": "b", "git": "https://x/b.git", "ref": "v1", "zz": true},
		{"name": "a", "enabled": false, "allow": ["fs"], "fs": {"roots": ["/var/log"]}}]}`
	if err := os.WriteFile(p, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if !f.HasPlugin("a") || f.HasPlugin("nope") {
		t.Fatal("HasPlugin")
	}
	if err := f.SetPluginEnabled("b", false); err != nil {
		t.Fatal(err)
	}
	if err := f.SetPluginEnabled("a", true); err != nil {
		t.Fatal(err)
	}
	if err := f.SetPluginEnabled("nope", true); err == nil {
		t.Fatal("unknown plugin must fail")
	}
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}
	f, _ = Open(p)
	ps, err := f.Plugins()
	if err != nil || len(ps) != 2 || ps[0] != (PluginState{"b", false}) || ps[1] != (PluginState{"a", true}) {
		t.Fatalf("%+v %v", ps, err)
	}
	data, _ := os.ReadFile(p)
	s := string(data)
	// key order inside each entry is kept; "enabled" stays where it was
	if !strings.Contains(s, `"custom": 1`) ||
		!(strings.Index(s, `"ref"`) < strings.Index(s, `"zz"`) && strings.Index(s, `"zz"`) < strings.Index(s, `"enabled": false`)) ||
		!(strings.Index(s, `"enabled": true`) < strings.Index(s, `"allow"`)) {
		t.Fatalf("file not preserved:\n%s", s)
	}
}
