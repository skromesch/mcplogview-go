package cli

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/skromesch/mcplogview-go/internal/config"
	"github.com/skromesch/mcplogview-go/internal/logs"
)

// answers is a plain pipe: all answers are available at once, like
// `printf '1\n2\n' | mcplogview configure`.
func answers(a ...string) io.Reader { return strings.NewReader(strings.Join(a, "\n") + "\n") }

func runWizard(t *testing.T, in io.Reader, args ...string) (code int, out string) {
	t.Helper()
	var o, e bytes.Buffer
	code = runConfigureWith(append([]string{"--plain"}, args...), in, &o, &e)
	return code, o.String() + e.String()
}

func browseFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, "sub"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "a.log"), []byte("a\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "sub", "inner.log"), []byte("i\n"), 0o644)
	old := browseStartDir
	browseStartDir = func(bool) string { return root }
	t.Cleanup(func() { browseStartDir = old })
	return root
}

func TestWizardAddLocalFileThroughBrowser(t *testing.T) {
	work := sandbox(t)
	root := browseFixture(t)

	code, out := runWizard(t, answers(
		"1",       // scope: this project
		"1",       // menu: add
		"1",       // local file
		"2",       // browser: sub/   (1=.. 2=sub/ 3=a.log 4=[manual] 5=cancel)
		"2",       // browser in sub: inner.log (1=.. 2=inner.log)
		"",        // id: default
		"",        // label: default
		"my logs", // description
		"",        // hint
		"7",       // menu: quit (add, toggle, remove, test, server, register, quit)
	))
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "✓ reachable and readable") || !strings.Contains(out, "✓ saved") {
		t.Fatalf("expected a successful check and save:\n%s", out)
	}

	cfg, err := config.Load(filepath.Join(work, config.LocalConfigName))
	if err != nil {
		t.Fatalf("written config does not load: %v", err)
	}
	var got config.Source
	for _, s := range cfg.Sources {
		if s.ID == "inner" {
			got = s
		}
	}
	if got.ID == "" || got.Path != filepath.Join(root, "sub", "inner.log") || got.Description != "my logs" || got.Label != "inner" {
		t.Fatalf("saved source wrong: %+v", got)
	}
}

func TestWizardRejectsDuplicateIDAndOffersSaveAnywayForBadSource(t *testing.T) {
	work := sandbox(t)
	browseFixture(t)
	_ = os.WriteFile(filepath.Join(work, config.LocalConfigName),
		[]byte(`{"sources":[{"id":"inner","label":"existing","path":"/etc/hosts"}]}`), 0o644)

	// existing config is used without asking for a scope; default id "inner" is
	// taken, so the wizard proposes inner-2
	code, out := runWizard(t, answers(
		"1", // add
		"1", // local
		"2", // sub/
		"2", // inner.log
		"",  // id: default -> inner-2
		"",  // label
		"",  // description
		"",  // hint
		"7", // quit
	))
	if code != 0 || !strings.Contains(out, "Editing ") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	f, _ := os.ReadFile(filepath.Join(work, config.LocalConfigName))
	if !strings.Contains(string(f), `"inner-2"`) || !strings.Contains(string(f), `"existing"`) {
		t.Fatalf("expected the old source kept and a new unique id:\n%s", f)
	}
}

func TestWizardUnreachableSourceNotSavedUnlessConfirmed(t *testing.T) {
	work := sandbox(t)
	browseFixture(t)

	// browser -> manual path to a file that does not exist; decline saving
	code, out := runWizard(t, answers(
		"1",                    // scope: project
		"1",                    // add
		"1",                    // local
		"4",                    // [type a path manually]
		"/definitely/not/here", // path
		"ghost",                // id
		"",                     // label
		"",                     // description
		"",                     // hint
		"n",                    // save anyway? no
		"4",                    // menu: quit (add, server, register, quit)
	))
	if code != 0 || !strings.Contains(out, "✗") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(work, config.LocalConfigName)); err == nil {
		t.Fatal("declined source must not create/write the config")
	}
}

func TestWizardStopsWhenInputEnds(t *testing.T) {
	sandbox(t)
	// a menu loop that keeps receiving "defaults" after EOF would spin forever
	done := make(chan int, 1)
	go func() {
		code, _ := runWizard(t, answers("1"))
		done <- code
	}()
	select {
	case code := <-done:
		if code == 0 {
			t.Fatal("running out of input must be an error, not success")
		}
	case <-timeAfter():
		t.Fatal("wizard did not stop after input EOF")
	}
}

func TestSlugAndDefaults(t *testing.T) {
	cases := map[string]string{"My API!": "my-api", "nginx_access.log": "nginx_access.log", "///": "log", "App Server": "app-server"}
	for in, want := range cases {
		if got := slug(in); got != want {
			t.Errorf("slug(%q)=%q want %q", in, got, want)
		}
	}
	if got := defaultName(config.Source{Type: config.SourceLocal, Path: "/var/log/nginx/access.log"}); got != "access" {
		t.Errorf("defaultName: %q", got)
	}
	if got := defaultName(config.Source{Type: config.SourceDocker, Container: "web-1"}); got != "web-1" {
		t.Errorf("defaultName docker: %q", got)
	}
}

func TestWizardAddsDockerContainerFromList(t *testing.T) {
	if exec.Command("docker", "info").Run() != nil || exec.Command("docker", "image", "inspect", "alpine:latest").Run() != nil {
		t.Skip("docker/alpine not available")
	}
	work := sandbox(t)
	name := strings.ReplaceAll("mcplogview-wiz-"+time.Now().Format("150405.000000"), ".", "-")
	if out, err := exec.Command("docker", "run", "-d", "--name", name, "alpine:latest", "sh", "-c", "echo hi; sleep 60").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	cs, err := logs.ListContainers(config.Source{})
	if err != nil {
		t.Fatal(err)
	}
	pos := 0
	for i, c := range cs {
		if c.Name == name {
			pos = i + 1
		}
	}
	if pos == 0 {
		t.Fatal("test container missing from the container list")
	}

	code, out := runWizard(t, answers(
		"1",               // scope: project
		"1",               // add
		"3",               // docker, this machine
		strconv.Itoa(pos), // pick the container from the list
		"web",             // id
		"",                // label
		"",                // description
		"",                // hint
		"7",               // quit
	))
	if code != 0 || !strings.Contains(out, "✓ reachable and readable") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	cfg, err := config.Load(filepath.Join(work, config.LocalConfigName))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range cfg.Sources {
		if s.ID == "web" && s.Type == config.SourceDocker && s.Container == name && s.Path == "" {
			return
		}
	}
	t.Fatalf("docker source not saved correctly: %+v", cfg.Sources)
}

func TestWizardTogglesSources(t *testing.T) {
	work := sandbox(t)
	_ = os.WriteFile(filepath.Join(work, config.LocalConfigName), []byte(`{"sources":[
		{"id":"a","label":"A","path":"/etc/hosts"},
		{"id":"b","label":"B","path":"/etc/hosts","disabled":true}]}`), 0o644)

	// menu: 2 = "Switch sources on / off". Plain mode toggles one number per
	// prompt and ends with 0: switch a off (1), b on (2).
	code, out := runWizard(t, answers("2", "1", "2", "0", "7"))
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	cfg, err := config.Load(filepath.Join(work, config.LocalConfigName))
	if err != nil {
		t.Fatal(err)
	}
	state := map[string]bool{}
	for _, s := range cfg.Sources {
		state[s.ID] = s.Disabled
	}
	if state["a"] != true || state["b"] != false {
		t.Fatalf("expected a off and b on, got disabled=%v\n%s", state, out)
	}
}

func TestWizardToggleWithoutChangesWritesNothing(t *testing.T) {
	work := sandbox(t)
	p := filepath.Join(work, config.LocalConfigName)
	orig := `{"sources":[{"id":"a","label":"A","path":"/etc/hosts"}]}`
	_ = os.WriteFile(p, []byte(orig), 0o644)

	code, out := runWizard(t, answers("2", "0", "7"))
	if code != 0 || !strings.Contains(out, "(no changes)") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if data, _ := os.ReadFile(p); string(data) != orig {
		t.Fatalf("file must stay untouched when nothing changed:\n%s", data)
	}
}

// Regression: without a height the multi-select rendered an empty box, so the
// user could not see (or sensibly toggle) the sources.
func TestToggleFieldRendersEverySource(t *testing.T) {
	for _, n := range []int{1, 2, 7} {
		var sources []config.Source
		for i := 0; i < n; i++ {
			sources = append(sources, config.Source{Type: config.SourceLocal, ID: "src" + strconv.Itoa(i), Label: "L", Path: "/p"})
		}
		ms, _ := newToggleField(sources)
		ms.Init()
		view := ms.View()
		for _, s := range sources {
			if !strings.Contains(view, s.ID) {
				t.Fatalf("%d source(s): %q missing from the rendered field:\n%s", n, s.ID, view)
			}
		}
	}
}

func TestWizardServerModeHTTP(t *testing.T) {
	work := sandbox(t)
	// no sources: menu = 1 add, 2 server mode, 3 register, 4 quit
	code, out := runWizard(t, answers(
		"1",    // scope: project
		"2",    // server mode
		"2",    // HTTP
		"1",    // this machine only
		"4567", // port
		"",     // path: default
		"1",    // token: generate
		"4",    // quit
	))
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	cfg, err := config.Load(filepath.Join(work, config.LocalConfigName))
	if err != nil {
		t.Fatal(err)
	}
	tr := cfg.Transport
	if tr.Mode != "http" || tr.Port != 4567 || tr.Host != "127.0.0.1" || tr.Path != "/mcp" || len(tr.AuthToken) < 16 {
		t.Fatalf("%+v", tr)
	}
	if !strings.Contains(out, "claude mcp add --transport http mcplogview http://127.0.0.1:4567/mcp") || !strings.Contains(out, tr.AuthToken) {
		t.Fatalf("registration instructions missing:\n%s", out)
	}
}

func TestWizardNetworkWithoutTokenNeedsConfirmation(t *testing.T) {
	work := sandbox(t)
	code, out := runWizard(t, answers(
		"1", // scope: project
		"2", // server mode
		"2", // HTTP
		"2", // all interfaces
		"",  // port: default
		"",  // path: default
		"2", // token: none
		"n", // really expose without token? no -> asked again
		"1", // token: generate
		"4", // quit
	))
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	cfg, err := config.Load(filepath.Join(work, config.LocalConfigName))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Transport.Host != "0.0.0.0" || cfg.Transport.AuthToken == "" || cfg.Transport.AllowNoAuth {
		t.Fatalf("declining must end with a token and no allowNoAuth: %+v", cfg.Transport)
	}
	if !strings.Contains(out, "unencrypted") {
		t.Fatalf("the plain-HTTP warning should be shown:\n%s", out)
	}
}

func TestBrowseStartDirEnvOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvBrowseDir, dir)
	if got := browseStartDir(false); got != dir {
		t.Fatalf("local browser should start in %s, got %s", dir, got)
	}
	if got := browseStartDir(true); got != "/var/log" {
		t.Fatalf("the override must not leak to remote browsing, got %s", got)
	}
}
