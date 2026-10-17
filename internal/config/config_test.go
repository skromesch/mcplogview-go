package config

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestResolveOrder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	cwd := t.TempDir()

	// nothing anywhere -> defaults
	if p, o, err := Resolve("", env(nil), cwd); err != nil || p != "" || o != OriginDefault {
		t.Fatalf("defaults: %q %q %v", p, o, err)
	}

	// user config is the second fallback
	user := UserConfigPath()
	write(t, user, `{}`)
	if p, o, _ := Resolve("", env(nil), cwd); p != user || o != OriginUser {
		t.Fatalf("user: %q %q", p, o)
	}

	// cwd beats user config
	local := filepath.Join(cwd, LocalConfigName)
	write(t, local, `{}`)
	if p, o, _ := Resolve("", env(nil), cwd); p != local || o != OriginCwd {
		t.Fatalf("cwd: %q %q", p, o)
	}

	// env beats cwd, flag beats env
	envFile := filepath.Join(home, "env.json")
	flagFile := filepath.Join(home, "flag.json")
	write(t, envFile, `{}`)
	write(t, flagFile, `{}`)
	if p, o, _ := Resolve("", env(map[string]string{EnvConfig: envFile}), cwd); p != envFile || o != OriginEnv {
		t.Fatalf("env: %q %q", p, o)
	}
	if p, o, _ := Resolve(flagFile, env(map[string]string{EnvConfig: envFile}), cwd); p != flagFile || o != OriginFlag {
		t.Fatalf("flag: %q %q", p, o)
	}

	// explicit but missing paths are errors, not silent fallbacks
	if _, _, err := Resolve(filepath.Join(home, "nope.json"), env(nil), cwd); err == nil {
		t.Fatal("expected error for missing --config")
	}
	if _, _, err := Resolve("", env(map[string]string{EnvConfig: filepath.Join(home, "nope.json")}), cwd); err == nil {
		t.Fatal("expected error for missing env config")
	}
}

func TestLoadAppliesDefaultsAndResolvesRelativePaths(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "mcplogview.json")
	write(t, cfgPath, `{"sources":[
		{"id":"app","label":"App","path":"logs/app.log"},
		{"type":"ssh","id":"r","label":"R","host":"h","user":"u","identityFile":"~/k","path":"/var/log/x"}
	]}`)

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Transport.Mode != TransportStdio || cfg.Transport.Port != 3333 {
		t.Fatalf("transport defaults: %+v", cfg.Transport)
	}
	// self-log source is prepended
	if len(cfg.Sources) != 3 || cfg.Sources[0].ID != "mcplogview-self" {
		t.Fatalf("sources: %+v", cfg.Sources)
	}
	app := cfg.Sources[1]
	if app.Type != SourceLocal || app.Path != filepath.Join(dir, "logs/app.log") {
		t.Fatalf("local source: %+v", app)
	}
	ssh := cfg.Sources[2]
	if app.Port != 0 {
		t.Fatalf("local sources must not get a default port: %+v", app)
	}
	if ssh.Port != 22 || ssh.Path != "/var/log/x" {
		t.Fatalf("ssh source must keep its remote path and default port: %+v", ssh)
	}
	home, _ := os.UserHomeDir()
	if ssh.IdentityFile != filepath.Join(home, "k") {
		t.Fatalf("identityFile ~ expansion: %q", ssh.IdentityFile)
	}
	if cfg.SkillsDir != filepath.Join(dir, "log-skills") {
		t.Fatalf("skillsDir: %q", cfg.SkillsDir)
	}
}

func TestLoadValidation(t *testing.T) {
	cases := map[string]string{
		"dup id":              `{"sources":[{"id":"a","label":"A","path":"/x"},{"id":"a","label":"B","path":"/y"}]}`,
		"bad transport":       `{"transport":{"mode":"carrier-pigeon"}}`,
		"ssh no host":         `{"sources":[{"type":"ssh","id":"a","label":"A","user":"u","identityFile":"k","path":"/x"}]}`,
		"unknown type":        `{"sources":[{"type":"ftp","id":"a","label":"A","path":"/x"}]}`,
		"bad json":            `{`,
		"docker no container": `{"sources":[{"type":"docker","id":"a","label":"A"}]}`,
		"docker bad name":     `{"sources":[{"type":"docker","id":"a","label":"A","container":"x; rm -rf /"}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "c.json")
			write(t, p, body)
			if _, err := Load(p); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestSelfSourceNotDuplicatedAndCanBeDisabled(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.json")

	write(t, p, `{"logging":{"enabled":false},"sources":[]}`)
	cfg, err := Load(p)
	if err != nil || len(cfg.Sources) != 0 {
		t.Fatalf("disabled self-log: %v %+v", err, cfg.Sources)
	}

	write(t, p, `{"sources":[{"id":"mcplogview-self","label":"mine","path":"/x"}]}`)
	cfg, err = Load(p)
	if err != nil || len(cfg.Sources) != 1 || cfg.Sources[0].Label != "mine" {
		t.Fatalf("user-defined self source must win: %v %+v", err, cfg.Sources)
	}
}

func TestDockerSources(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	write(t, p, `{"logging":{"enabled":false},"sources":[
		{"type":"docker","id":"l","label":"L","container":"my-api"},
		{"type":"docker","id":"r","label":"R","container":"api_1.x","host":"prod"}]}`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sources[0].Remote() || !cfg.Sources[1].Remote() {
		t.Fatalf("Remote(): %+v", cfg.Sources)
	}
	if cfg.Sources[1].Port != 0 {
		t.Fatalf("docker remote must leave the port to ssh config: %+v", cfg.Sources[1])
	}
}

func TestDisabledSources(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	write(t, p, `{"logging":{"enabled":false},"sources":[
		{"id":"on","label":"On","path":"/a"},
		{"id":"off","label":"Off","path":"/b","disabled":true}]}`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Sources) != 2 {
		t.Fatalf("disabled sources stay in the config: %+v", cfg.Sources)
	}
	en := cfg.EnabledSources()
	if len(en) != 1 || en[0].ID != "on" {
		t.Fatalf("EnabledSources: %+v", en)
	}
}

func TestTransportAuthHelpers(t *testing.T) {
	for host, want := range map[string]bool{"127.0.0.1": true, "localhost": true, "::1": true, "[::1]": true, "127.1.2.3": true,
		"0.0.0.0": false, "::": false, "192.168.1.5": false, "myhost.lan": false, "": false} {
		if got := (Transport{Host: host}).IsLoopback(); got != want {
			t.Errorf("IsLoopback(%q)=%v want %v", host, got, want)
		}
	}
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if got := ResolveToken("fromconfig", env(nil)); got != "fromconfig" {
		t.Errorf("config token: %q", got)
	}
	if got := ResolveToken("fromconfig", env(map[string]string{EnvToken: "fromenv"})); got != "fromenv" {
		t.Errorf("env must win: %q", got)
	}

	p := filepath.Join(t.TempDir(), "c.json")
	write(t, p, `{"transport":{"mode":"http","authToken":"short"}}`)
	if _, err := Load(p); err == nil {
		t.Fatal("a too-short token must be rejected")
	}
}
