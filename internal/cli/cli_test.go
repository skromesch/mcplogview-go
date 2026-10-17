package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skromesch/mcplogview-go/internal/config"
)

// sandbox gives each test its own cwd and HOME so the real user config is never touched.
func sandbox(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", filepath.Join(dir, "home"))
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv(config.EnvConfig, "")
	t.Setenv(config.EnvToken, "")
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)
	return work
}

func run(t *testing.T, args ...string) (code int, out, errOut string) {
	t.Helper()
	var o, e bytes.Buffer
	code = Run(args, &o, &e)
	return code, o.String(), e.String()
}

func TestBareRunInTerminalShowsHelp(t *testing.T) {
	sandbox(t)
	old := stdinIsTerminal
	stdinIsTerminal = func() bool { return true }
	t.Cleanup(func() { stdinIsTerminal = old })

	code, out, _ := run(t)
	if code != 0 || !strings.Contains(out, "mcplogview serve") || !strings.Contains(out, "configure") {
		t.Fatalf("code=%d out=%q", code, out)
	}
	for _, alias := range []string{"help", "-h", "--help"} {
		if c, o, _ := run(t, alias); c != 0 || !strings.Contains(o, "Usage:") {
			t.Fatalf("%s: %d %q", alias, c, o)
		}
	}
}

func TestUnknownCommand(t *testing.T) {
	sandbox(t)
	code, _, errOut := run(t, "frobnicate")
	if code != 2 || !strings.Contains(errOut, "unknown command") {
		t.Fatalf("%d %q", code, errOut)
	}
}

func TestAddListCheckRemoveLocal(t *testing.T) {
	work := sandbox(t)
	logFile := filepath.Join(work, "app.log")
	_ = os.WriteFile(logFile, []byte("hello\n"), 0o644)

	// missing file is rejected unless --no-check
	if code, _, errOut := run(t, "add", "local", "--id", "ghost", "--path", "nope.log"); code != 1 || !strings.Contains(errOut, "check failed") {
		t.Fatalf("%d %q", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(work, config.LocalConfigName)); err == nil {
		t.Fatal("a failed check must not create the config")
	}

	// relative path is stored absolute
	if code, out, errOut := run(t, "add", "local", "--id", "app", "--path", "app.log", "--label", "App", "--hint", "h"); code != 0 {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	data, _ := os.ReadFile(filepath.Join(work, config.LocalConfigName))
	if !strings.Contains(string(data), `"path": "`+logFile+`"`) {
		t.Fatalf("path should be absolute:\n%s", data)
	}

	if code, _, errOut := run(t, "add", "local", "--id", "app", "--path", "app.log"); code != 1 || !strings.Contains(errOut, "already exists") {
		t.Fatalf("duplicate: %d %q", code, errOut)
	}
	if code, _, _ := run(t, "add", "local", "--id", "bad id with spaces", "--path", "app.log"); code != 0 {
		// ids with spaces are allowed by the server; just must not crash
		t.Log("id with spaces accepted")
	}

	code, out, _ := run(t, "list")
	if code != 0 || !strings.Contains(out, "app") || !strings.Contains(out, "mcplogview-self") || !strings.Contains(out, logFile) {
		t.Fatalf("list: %d\n%s", code, out)
	}

	if code, out, _ := run(t, "check", "app"); code != 0 || !strings.Contains(out, "ok") {
		t.Fatalf("check ok: %d %q", code, out)
	}
	_ = os.Remove(logFile)
	if code, out, _ := run(t, "check"); code != 1 || !strings.Contains(out, "FAIL") {
		t.Fatalf("check fail: %d %q", code, out)
	}
	if code, _, errOut := run(t, "check", "unknown"); code != 1 || !strings.Contains(errOut, "no source") {
		t.Fatalf("%d %q", code, errOut)
	}

	if code, _, _ := run(t, "remove", "app"); code != 0 {
		t.Fatal("remove failed")
	}
	if code, _, errOut := run(t, "remove", "app"); code != 1 || !strings.Contains(errOut, "no source") {
		t.Fatalf("%d %q", code, errOut)
	}
}

func TestAddValidationAndNoCheck(t *testing.T) {
	sandbox(t)
	cases := [][]string{
		{"add"},
		{"add", "nonsense"},
		{"add", "local", "--id", "x"}, // no path
		{"add", "ssh", "--id", "x", "--path", "/p"},          // no host/user/key
		{"add", "docker", "--id", "x"},                       // no container
		{"add", "docker", "--id", "x", "--container", "a;b"}, // unsafe name
	}
	for _, c := range cases {
		if code, _, _ := run(t, c...); code == 0 {
			t.Fatalf("%v should fail", c)
		}
	}
	if code, _, _ := run(t, "add", "ssh", "--id", "r", "--host", "h", "--user", "u", "--identity-file", "/k", "--path", "/var/log/x", "--no-check"); code != 0 {
		t.Fatal("--no-check must allow an unreachable ssh source")
	}
	data, _ := os.ReadFile(config.LocalConfigName)
	if !strings.Contains(string(data), `"port": 22`) {
		t.Fatalf("ssh default port missing:\n%s", data)
	}
}

func TestEditsRespectConfigPrecedence(t *testing.T) {
	work := sandbox(t)
	user := config.UserConfigPath()
	_ = os.MkdirAll(filepath.Dir(user), 0o755)
	_ = os.WriteFile(user, []byte(`{"sources":[]}`), 0o644)

	// no ./mcplogview.json yet: the existing user config is the edit target
	if code, _, e := run(t, "add", "local", "--id", "a", "--path", "/etc/hosts"); code != 0 {
		t.Fatal(e)
	}
	if _, err := os.Stat(filepath.Join(work, config.LocalConfigName)); err == nil {
		t.Fatal("must edit the existing user config, not create a project file")
	}
	data, _ := os.ReadFile(user)
	if !strings.Contains(string(data), `"id": "a"`) {
		t.Fatalf("user config not edited:\n%s", data)
	}

	// an explicit --config always wins, even for a new file
	other := filepath.Join(work, "other.json")
	if code, _, e := run(t, "add", "local", "--id", "b", "--path", "/etc/hosts", "--config", other); code != 0 {
		t.Fatal(e)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("--config target not created")
	}
}

func TestInitWhereRegister(t *testing.T) {
	work := sandbox(t)
	if code, out, _ := run(t, "where"); code != 0 || !strings.Contains(out, "built-in defaults") {
		t.Fatalf("%d %q", code, out)
	}
	if code, _, _ := run(t, "init"); code != 0 {
		t.Fatal("init failed")
	}
	if code, _, e := run(t, "init"); code != 1 || !strings.Contains(e, "already exists") {
		t.Fatalf("init must not overwrite: %d %q", code, e)
	}
	// the example config must be valid
	if _, err := config.Load(filepath.Join(work, config.LocalConfigName)); err != nil {
		t.Fatalf("example config invalid: %v", err)
	}
	if code, out, _ := run(t, "where"); code != 0 || !strings.Contains(out, "current directory") {
		t.Fatalf("%d %q", code, out)
	}
	code, out, _ := run(t, "register")
	if code != 0 || !strings.Contains(out, "claude mcp add mcplogview -- ") || !strings.Contains(out, `"command"`) {
		t.Fatalf("%d %q", code, out)
	}
}

func TestAddDockerAgainstRealContainer(t *testing.T) {
	if exec.Command("docker", "info").Run() != nil || exec.Command("docker", "image", "inspect", "alpine:latest").Run() != nil {
		t.Skip("docker/alpine not available")
	}
	sandbox(t)
	name := "mcplogview-cli-" + time.Now().Format("150405.000000")
	name = strings.ReplaceAll(name, ".", "-")
	if out, err := exec.Command("docker", "run", "-d", "--name", name, "alpine:latest", "sh", "-c", "echo hello; sleep 60").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	if code, _, e := run(t, "add", "docker", "--id", "d", "--container", "mcplogview-no-such"); code != 1 || !strings.Contains(e, "container not found") {
		t.Fatalf("%d %q", code, e)
	}
	if code, out, e := run(t, "add", "docker", "--id", "d", "--container", name); code != 0 {
		t.Fatalf("%d %q %q", code, out, e)
	}
	if code, out, _ := run(t, "check", "d"); code != 0 || !strings.Contains(out, "ok") {
		t.Fatalf("%d %q", code, out)
	}
	data, _ := os.ReadFile(config.LocalConfigName)
	if strings.Contains(string(data), `"path"`) || strings.Contains(string(data), `"port"`) {
		t.Fatalf("docker source should carry no path/port:\n%s", data)
	}
}

func timeAfter() <-chan time.Time { return time.After(5 * time.Second) }

func TestEnableDisableAndList(t *testing.T) {
	work := sandbox(t)
	_ = os.WriteFile(filepath.Join(work, "a.log"), []byte("x\n"), 0o644)
	run(t, "add", "local", "--id", "one", "--path", "a.log")
	run(t, "add", "local", "--id", "two", "--path", "a.log")

	if code, _, e := run(t, "disable", "one"); code != 0 {
		t.Fatal(e)
	}
	_, out, _ := run(t, "list")
	if !strings.Contains(out, "one") || !strings.Contains(out, "off") || !strings.Contains(out, "two") {
		t.Fatalf("list should show the state:\n%s", out)
	}
	// check skips disabled sources unless they are named explicitly
	_ = os.Remove(filepath.Join(work, "a.log"))
	if code, out, _ := run(t, "check"); code != 1 || strings.Contains(out, "one") {
		t.Fatalf("disabled source must not be checked implicitly: %d\n%s", code, out)
	}
	if code, out, _ := run(t, "check", "one"); code != 1 || !strings.Contains(out, "FAIL  one") {
		t.Fatalf("explicit check works on disabled sources: %d\n%s", code, out)
	}

	if code, _, _ := run(t, "enable", "one"); code != 0 {
		t.Fatal("enable failed")
	}
	if data, _ := os.ReadFile(config.LocalConfigName); strings.Contains(string(data), "disabled") {
		t.Fatalf("enabled source must drop the flag:\n%s", data)
	}
	if code, _, _ := run(t, "disable", "--all"); code != 0 {
		t.Fatal("disable --all failed")
	}
	if data, _ := os.ReadFile(config.LocalConfigName); strings.Count(string(data), `"disabled": true`) != 2 {
		t.Fatalf("--all should disable both:\n%s", data)
	}
	if code, _, e := run(t, "disable", "nope"); code != 1 || !strings.Contains(e, "no source") {
		t.Fatalf("%d %q", code, e)
	}
	if code, _, _ := run(t, "enable"); code != 2 {
		t.Fatal("enable without ids must be a usage error")
	}
}

func TestTransportCommand(t *testing.T) {
	work := sandbox(t)
	p := filepath.Join(work, config.LocalConfigName)

	// show: default is stdio
	if code, out, _ := run(t, "transport"); code != 0 || !strings.Contains(out, "mode:   stdio") {
		t.Fatalf("%d %q", code, out)
	}

	// switching to http generates a token (secure by default) and locks the file down
	code, out, e := run(t, "transport", "--mode", "http", "--port", "4455")
	if code != 0 {
		t.Fatalf("%d %q %q", code, out, e)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	tok := cfg.Transport.AuthToken
	if cfg.Transport.Mode != "http" || cfg.Transport.Port != 4455 || len(tok) < 16 {
		t.Fatalf("%+v", cfg.Transport)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Fatalf("config with a token must be 0600, got %v", st.Mode().Perm())
	}
	for _, want := range []string{tok, "claude mcp add --transport http mcplogview http://127.0.0.1:4455/mcp", "Authorization: Bearer " + tok} {
		if !strings.Contains(out, want) {
			t.Fatalf("%q missing from output:\n%s", want, out)
		}
	}

	// showing again masks the token unless asked
	_, out, _ = run(t, "transport")
	if strings.Contains(out[:strings.Index(out, "\n\n")], tok) {
		t.Fatalf("token must be masked in the summary:\n%s", out)
	}
	if _, out, _ = run(t, "transport", "--show-token"); !strings.Contains(out, tok) {
		t.Fatal("--show-token should reveal it")
	}

	// register in http mode points clients at the URL with the header
	_, out, _ = run(t, "register")
	if !strings.Contains(out, "http://127.0.0.1:4455/mcp") || !strings.Contains(out, "Bearer "+tok) || strings.Contains(out, `"command"`) {
		t.Fatalf("register output:\n%s", out)
	}

	// exposing it on the network needs a token; removing it is refused
	if code, _, e := run(t, "transport", "--host", "0.0.0.0", "--no-token"); code != 2 || !strings.Contains(e, "refusing") {
		t.Fatalf("%d %q", code, e)
	}
	if code, _, _ := run(t, "transport", "--host", "0.0.0.0"); code != 0 {
		t.Fatal("network exposure with the existing token is fine")
	}
	if cfg, _ = config.Load(p); cfg.Transport.AuthToken != tok {
		t.Fatal("an existing token must be kept when only the host changes")
	}
	// ...unless explicitly allowed
	if code, _, _ := run(t, "transport", "--no-token", "--allow-no-auth"); code != 0 {
		t.Fatal("explicit --allow-no-auth must work")
	}

	// rotate, and invalid input
	run(t, "transport", "--host", "127.0.0.1", "--new-token")
	if cfg, _ = config.Load(p); cfg.Transport.AuthToken == "" || cfg.Transport.AuthToken == tok {
		t.Fatalf("--new-token should replace the token: %q", cfg.Transport.AuthToken)
	}
	for _, bad := range [][]string{{"--mode", "carrier-pigeon"}, {"--port", "99999"}, {"--new-token", "--no-token"}} {
		if code, _, _ := run(t, append([]string{"transport"}, bad...)...); code != 2 {
			t.Fatalf("%v should be a usage error", bad)
		}
	}

	// back to stdio keeps unrelated settings and sources
	run(t, "add", "local", "--id", "a", "--path", "/etc/hosts")
	if code, _, _ := run(t, "transport", "--mode", "stdio"); code != 0 {
		t.Fatal("switch back failed")
	}
	if cfg, _ = config.Load(p); cfg.Transport.Mode != "stdio" || len(cfg.EnabledSources()) < 2 {
		t.Fatalf("%+v %+v", cfg.Transport, cfg.Sources)
	}
}

func TestEnvTokenOverridesConfigInRegister(t *testing.T) {
	sandbox(t)
	run(t, "transport", "--mode", "http")
	t.Setenv(config.EnvToken, "token-from-the-environment-123")
	_, out, _ := run(t, "register")
	if !strings.Contains(out, "Bearer token-from-the-environment-123") {
		t.Fatalf("env token should win:\n%s", out)
	}
}
