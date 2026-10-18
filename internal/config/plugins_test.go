package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestPluginsAbsentKeepsDefaults(t *testing.T) {
	cfg := Defaults()
	if err := cfg.Finalize(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Plugins) != 0 {
		t.Fatalf("Plugins = %v, want none", cfg.Plugins)
	}
}

func TestPluginDirsDefault(t *testing.T) {
	base := t.TempDir()
	cfg := Defaults()
	if err := cfg.Finalize(base); err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(base, "plugins")}
	if len(cfg.PluginDirs) != 1 || cfg.PluginDirs[0] != want[0] {
		t.Fatalf("PluginDirs = %v, want %v", cfg.PluginDirs, want)
	}
}

func okPlugin() PluginRef {
	return PluginRef{
		Name:  "stations",
		Allow: []string{"ssh", "fs"},
		SSH:   &PluginSSH{AllowedCIDRs: []string{"10.0.0.0/24"}, User: "root", IdentityFile: "k"},
		FS:    &PluginFS{Roots: []string{"/var/log"}},
	}
}

func TestPluginValidation(t *testing.T) {
	cases := map[string]func(p *PluginRef){
		"uppercase name":    func(p *PluginRef) { p.Name = "Stations" },
		"space in name":     func(p *PluginRef) { p.Name = "ol gi" },
		"empty name":        func(p *PluginRef) { p.Name = "" },
		"unknown allow":     func(p *PluginRef) { p.Allow = []string{"exec"} },
		"ssh without block": func(p *PluginRef) { p.SSH = nil },
		"bad cidr":          func(p *PluginRef) { p.SSH.AllowedCIDRs = []string{"10.0.0.0/33"} },
		"no cidr":           func(p *PluginRef) { p.SSH.AllowedCIDRs = nil },
		"empty user":        func(p *PluginRef) { p.SSH.User = "" },
		"no identity":       func(p *PluginRef) { p.SSH.IdentityFile = "" },
		"bad port":          func(p *PluginRef) { p.SSH.Port = 70000 },
		"fs without block":  func(p *PluginRef) { p.FS = nil },
		"fs without roots":  func(p *PluginRef) { p.FS.Roots = nil },
		"timeout too big":   func(p *PluginRef) { p.TimeoutSeconds = 601 },
		"negative timeout":  func(p *PluginRef) { p.TimeoutSeconds = -1 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := okPlugin()
			mutate(&p)
			cfg := Defaults()
			cfg.Plugins = []PluginRef{p}
			if err := cfg.Finalize(t.TempDir()); err == nil {
				t.Fatal("expected error")
			}
		})
	}
	t.Run("duplicate name", func(t *testing.T) {
		cfg := Defaults()
		cfg.Plugins = []PluginRef{okPlugin(), okPlugin()}
		err := cfg.Finalize(t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "duplicate") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("valid", func(t *testing.T) {
		cfg := Defaults()
		cfg.Plugins = []PluginRef{okPlugin()}
		if err := cfg.Finalize(t.TempDir()); err != nil {
			t.Fatal(err)
		}
	})
}

func TestPluginPathsResolveAgainstConfigDir(t *testing.T) {
	base := t.TempDir()
	p := okPlugin()
	p.SSH.IdentityFile = "keys/id"
	p.FS.Roots = []string{"data", "/abs/root"}
	cfg := Defaults()
	cfg.PluginDirs = []string{"mine"}
	cfg.Plugins = []PluginRef{p}
	if err := cfg.Finalize(base); err != nil {
		t.Fatal(err)
	}
	got := cfg.Plugins[0]
	if cfg.PluginDirs[0] != filepath.Join(base, "mine") {
		t.Errorf("PluginDirs = %v", cfg.PluginDirs)
	}
	if got.SSH.IdentityFile != filepath.Join(base, "keys/id") {
		t.Errorf("identityFile = %q", got.SSH.IdentityFile)
	}
	if got.FS.Roots[0] != filepath.Join(base, "data") || got.FS.Roots[1] != "/abs/root" {
		t.Errorf("roots = %v", got.FS.Roots)
	}
	if got.SSH.Port != 22 {
		t.Errorf("port = %d, want 22", got.SSH.Port)
	}
}

func TestPluginEnabledAndAllows(t *testing.T) {
	var p PluginRef
	if !p.IsEnabled() {
		t.Error("default should be enabled")
	}
	f := false
	p.Enabled = &f
	if p.IsEnabled() {
		t.Error("enabled=false ignored")
	}
	p.Allow = []string{"ssh"}
	if !p.Allows("ssh") || p.Allows("fs") {
		t.Error("Allows wrong")
	}
}

func TestPluginGitValidation(t *testing.T) {
	bad := map[string]PluginRef{
		"git whitespace":  {Name: "x", Git: "https://h/a b.git"},
		"git dash":        {Name: "x", Git: "-uhttps://h/a.git"},
		"git nul":         {Name: "x", Git: "https://h/a\x00.git"},
		"ref whitespace":  {Name: "x", Git: "https://h/a.git", Ref: "v 1"},
		"ref dash":        {Name: "x", Git: "https://h/a.git", Ref: "--upload-pack=x"},
		"ref without git": {Name: "x", Ref: "main"},
	}
	for name, p := range bad {
		t.Run(name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Plugins = []PluginRef{p}
			if err := cfg.Finalize(t.TempDir()); err == nil {
				t.Fatal("expected error")
			}
		})
	}
	cfg := Defaults()
	cfg.Plugins = []PluginRef{{Name: "x", Git: "https://h/a.git", Ref: "v1.0"}}
	if err := cfg.Finalize(t.TempDir()); err != nil {
		t.Fatal(err)
	}
}
