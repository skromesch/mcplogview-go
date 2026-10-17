package configure

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skromesch/mcplogview-go/internal/config"
)

func TestAddRemovePreservesOtherSettings(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "mcplogview.json")
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, []byte(`{
  "custom": {"keep": [1, 2]},
  "transport": {"mode": "http", "port": 4444},
  "sources": [{"id": "old", "label": "Old", "path": "/x.log"}]
}`), 0o644)

	f, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Add(config.Source{Type: config.SourceDocker, ID: "api", Label: "API", Container: "my-api", Path: "junk"}); err != nil {
		t.Fatal(err)
	}
	if err := f.Add(config.Source{ID: "old", Label: "dup", Path: "/y"}); err == nil {
		t.Fatal("duplicate id must be rejected")
	}
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(p)
	var generic map[string]json.RawMessage
	if err := json.Unmarshal(data, &generic); err != nil {
		t.Fatalf("saved file is not valid JSON: %v\n%s", err, data)
	}
	if !strings.Contains(string(generic["custom"]), `"keep"`) || !strings.Contains(string(generic["transport"]), "4444") {
		t.Fatalf("unmanaged settings lost:\n%s", data)
	}
	if strings.Index(string(data), `"transport"`) > strings.Index(string(data), `"sources"`) {
		t.Fatalf("sources should come after transport:\n%s", data)
	}
	if strings.Contains(string(data), `"junk"`) || strings.Contains(string(data), `"port"`) && strings.Contains(string(data), `"container": "my-api"`) && strings.Count(string(data), `"port"`) > 1 {
		t.Fatalf("inapplicable fields must be dropped:\n%s", data)
	}

	// the result must load through the real loader
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("saved config does not load: %v\n%s", err, data)
	}
	ids := []string{}
	for _, s := range cfg.Sources {
		ids = append(ids, s.ID)
	}
	if strings.Join(ids, ",") != "mcplogview-self,old,api" {
		t.Fatalf("sources: %v", ids)
	}

	f2, _ := Open(p)
	if err := f2.Remove("old"); err != nil {
		t.Fatal(err)
	}
	if err := f2.Remove("nope"); err == nil {
		t.Fatal("removing an unknown id must fail")
	}
	_ = f2.Save()
	if f3, _ := Open(p); len(f3.Sources) != 1 || f3.Sources[0].ID != "api" {
		t.Fatalf("%+v", f3.Sources)
	}
}

func TestOpenMissingFileAndNormalize(t *testing.T) {
	f, err := Open(filepath.Join(t.TempDir(), "new.json"))
	if err != nil || len(f.Sources) != 0 {
		t.Fatalf("%v %+v", err, f)
	}
	if err := f.Add(config.Source{ID: "a", Label: "A", Path: "/a.log", Port: 99, Host: "stale"}); err != nil {
		t.Fatal(err)
	}
	if s := f.Sources[0]; s.Port != 0 || s.Host != "" || s.Type != config.SourceLocal {
		t.Fatalf("local source not normalized: %+v", s)
	}
	if err := f.Add(config.Source{Type: config.SourceSSH, ID: "s", Label: "S", Host: "h", User: "u", IdentityFile: "/k", Path: "/p"}); err != nil {
		t.Fatal(err)
	}
	if f.Sources[1].Port != 22 {
		t.Fatalf("ssh default port: %+v", f.Sources[1])
	}
	if err := f.Add(config.Source{Type: config.SourceDocker, ID: "d", Label: "D", Container: "bad name;"}); err == nil {
		t.Fatal("invalid container must be rejected")
	}
}

func TestOpenRejectsBrokenJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	_ = os.WriteFile(p, []byte(`{ not json`), 0o644)
	if _, err := Open(p); err == nil {
		t.Fatal("expected error; a broken file must never be silently overwritten")
	}
}

func TestParseSSHG(t *testing.T) {
	home, _ := os.UserHomeDir()
	_ = home
	out := []byte("host prod\nhostname 10.0.0.12\nuser ubuntu\nport 2222\nidentityfile ~/.ssh/definitely-not-here\nidentityfile /etc/hosts\n")
	h := parseSSHG("prod", out)
	if h.HostName != "10.0.0.12" || h.User != "ubuntu" || h.Port != 2222 {
		t.Fatalf("%+v", h)
	}
	if h.IdentityFile != "/etc/hosts" {
		t.Fatalf("must pick the first identity file that exists: %q", h.IdentityFile)
	}
}

func TestSetDisabledRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	f, _ := Open(p)
	_ = f.Add(config.Source{ID: "a", Label: "A", Path: "/a"})
	_ = f.Add(config.Source{ID: "b", Label: "B", Path: "/b"})
	if err := f.SetDisabled("b", true); err != nil {
		t.Fatal(err)
	}
	if err := f.SetDisabled("zzz", true); err == nil {
		t.Fatal("unknown id must fail")
	}
	_ = f.Save()

	data, _ := os.ReadFile(p)
	if strings.Count(string(data), `"disabled"`) != 1 {
		t.Fatalf("only the disabled source carries the flag:\n%s", data)
	}
	g, _ := Open(p)
	_ = g.SetDisabled("b", false)
	_ = g.Save()
	data, _ = os.ReadFile(p)
	if strings.Contains(string(data), "disabled") {
		t.Fatalf("re-enabling removes the key again:\n%s", data)
	}
}

func TestUpdateTransportKeepsOtherKeysAndLocksDownTokenFiles(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	_ = os.WriteFile(p, []byte(`{"transport":{"mode":"stdio","httpsKey":"/k.pem","futureKey":{"a":1}},"sources":[]}`), 0o644)

	f, _ := Open(p)
	if err := f.UpdateTransport(map[string]any{"mode": "http", "port": 4000, "authToken": "0123456789abcdef0123"}); err != nil {
		t.Fatal(err)
	}
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	for _, want := range []string{`"futureKey"`, `"httpsKey"`, `"port": 4000`, `"mode": "http"`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("%s lost:\n%s", want, data)
		}
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Fatalf("a file with a token must be owner-only, got %v", st.Mode().Perm())
	}
	g, _ := Open(p)
	tr, _ := g.Transport()
	if tr.Mode != "http" || tr.Port != 4000 || tr.AuthToken == "" || tr.Host != "127.0.0.1" {
		t.Fatalf("Transport(): %+v", tr)
	}

	// removing the token (nil) deletes the key; permissions are never widened
	_ = g.UpdateTransport(map[string]any{"authToken": nil})
	_ = g.Save()
	data, _ = os.ReadFile(p)
	if strings.Contains(string(data), "authToken") {
		t.Fatalf("token key should be gone:\n%s", data)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Fatalf("existing restrictive permissions must be kept, got %v", st.Mode().Perm())
	}
}

func TestNewTokenIsRandomAndLongEnough(t *testing.T) {
	a, _ := NewToken()
	b, _ := NewToken()
	if a == b || len(a) < 16 || strings.ContainsAny(a, "+/= ") {
		t.Fatalf("%q %q", a, b)
	}
}
