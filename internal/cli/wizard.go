package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"charm.land/huh/v2"

	"github.com/skromesch/mcplogview-go/internal/config"
	"github.com/skromesch/mcplogview-go/internal/configure"
	"github.com/skromesch/mcplogview-go/internal/logs"
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// ui wraps huh so the wizard can run interactively or, for pipes and
// screen readers, in huh's plain "accessible" mode.
type ui struct {
	in    *eofReader
	out   io.Writer
	plain bool
}

// eofReader hands out one line per Read and remembers that the input ran dry.
//
// huh's plain mode opens a fresh bufio.Scanner per prompt; on a pipe the first
// scanner would swallow every later answer, so the lines must be released one
// at a time. In plain mode huh also answers a starved prompt with its default,
// which would loop the menu forever, hence the eof flag.
type eofReader struct {
	br      *bufio.Reader
	pending []byte
	eof     bool
}

func newEOFReader(r io.Reader) *eofReader { return &eofReader{br: bufio.NewReader(r)} }

func (e *eofReader) Read(p []byte) (int, error) {
	if len(e.pending) == 0 {
		line, err := e.br.ReadBytes('\n')
		if len(line) == 0 && err != nil {
			if err == io.EOF {
				e.eof = true
			}
			return 0, err
		}
		e.pending = line
	}
	n := copy(p, e.pending)
	e.pending = e.pending[n:]
	return n, nil
}

type choice struct{ label, value string }

func (u *ui) run(fields ...huh.Field) (err error) {
	// A prompt library panic (seen in huh's plain multi-select on exhausted
	// input) must end the prompt, not the whole program.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("prompt failed: %v", r)
		}
	}()
	f := huh.NewForm(huh.NewGroup(fields...)).WithAccessible(u.plain)
	if u.plain {
		// Only plain prompts read through our line-wise reader. The TUI must get
		// the real terminal (*os.File): wrapping it would stop Bubble Tea from
		// switching the terminal to raw mode and keys would arrive line-buffered.
		f = f.WithOutput(u.out)
		if u.in != nil {
			f = f.WithInput(u.in)
		}
	}
	if err := f.Run(); err != nil {
		return err
	}
	if u.plain && u.in != nil && u.in.eof {
		return errInputClosed
	}
	return nil
}

func (u *ui) printf(format string, a ...any) { fmt.Fprintf(u.out, format, a...) }

func (u *ui) selectOne(title, desc string, opts []choice, filtering bool) (string, error) {
	var v string
	hopts := make([]huh.Option[string], len(opts))
	for i, o := range opts {
		hopts[i] = huh.NewOption(o.label, o.value)
	}
	h := len(opts) + 3
	if h > 18 {
		h = 18
	}
	sel := huh.NewSelect[string]().Title(title).Description(desc).Options(hopts...).Value(&v).Filtering(filtering).Height(h)
	if err := u.run(sel); err != nil {
		return "", err
	}
	return v, nil
}

func (u *ui) input(title, def string, validate func(string) error) (string, error) {
	v := def
	in := huh.NewInput().Title(title).Value(&v)
	if validate != nil {
		// huh's plain mode validates the raw answer before applying the
		// default, so an empty Enter must be judged as the default value.
		in = in.Validate(func(s string) error {
			if strings.TrimSpace(s) == "" && def != "" {
				s = def
			}
			return validate(s)
		})
	}
	if err := u.run(in); err != nil {
		return "", err
	}
	return strings.TrimSpace(v), nil
}

func (u *ui) confirm(title string, def bool) (bool, error) {
	v := def
	if err := u.run(huh.NewConfirm().Title(title).Value(&v)); err != nil {
		return false, err
	}
	return v, nil
}

func required(name string) func(string) error {
	return func(s string) error {
		if strings.TrimSpace(s) == "" {
			return fmt.Errorf("%s is required", name)
		}
		return nil
	}
}

type wizard struct {
	ui     *ui
	file   *configure.File
	stderr io.Writer
}

func runConfigure(args []string, stdout, stderr io.Writer) int {
	return runConfigureWith(args, os.Stdin, stdout, stderr)
}

func runConfigureWith(args []string, in io.Reader, stdout, stderr io.Writer) int {
	fs, configPath := newFlags("configure", stderr)
	plainFlag := fs.Bool("plain", false, "plain prompts (no TUI); automatic when stdin is not a terminal")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	plain := *plainFlag || os.Getenv("MCPLOGVIEW_PLAIN") != "" || os.Getenv("TERM") == "dumb" || !stdinIsTerminal()
	w := &wizard{ui: &ui{in: newEOFReader(in), out: stdout, plain: plain}, stderr: stderr}

	target, err := w.chooseTarget(*configPath)
	if err != nil {
		return w.finish(err)
	}
	w.file, err = configure.Open(target)
	if err != nil {
		fmt.Fprintln(stderr, "mcplogview:", err)
		return 1
	}
	return w.finish(w.menu())
}

func (w *wizard) finish(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, huh.ErrUserAborted):
		fmt.Fprintln(w.stderr, "\naborted")
		return 130
	default:
		fmt.Fprintln(w.stderr, "mcplogview:", err)
		return 1
	}
}

// chooseTarget picks the file to edit; it only asks when nothing exists yet.
func (w *wizard) chooseTarget(flagPath string) (string, error) {
	cwd, _ := os.Getwd()
	existing, origin, err := config.Resolve(flagPath, os.Getenv, cwd)
	if err != nil && flagPath == "" {
		return "", err
	}
	if flagPath != "" {
		return absPath(flagPath), nil
	}
	if existing != "" {
		w.ui.printf("Editing %s (%s)\n\n", absPath(existing), origin)
		return absPath(existing), nil
	}
	v, err := w.ui.selectOne("No config file yet. Where should it live?", "", []choice{
		{"This project: " + filepath.Join(cwd, config.LocalConfigName), "project"},
		{"User-wide: " + config.UserConfigPath(), "user"},
	}, false)
	if err != nil {
		return "", err
	}
	if v == "user" {
		return config.UserConfigPath(), nil
	}
	return filepath.Join(cwd, config.LocalConfigName), nil
}

func (w *wizard) menu() error {
	for {
		w.ui.printf("\nConfig: %s\n", w.file.Path)
		if len(w.file.Sources) == 0 {
			w.ui.printf("  (no sources yet)\n")
		}
		for _, s := range w.file.Sources {
			w.ui.printf("  %s %-18s %-7s %s\n", map[bool]string{false: "[x]", true: "[ ]"}[s.Disabled], s.ID, s.Type, Describe(s))
		}
		w.ui.printf("\n")

		opts := []choice{{"Add a source", "add"}}
		if len(w.file.Sources) > 0 {
			opts = append(opts, choice{"Switch sources on / off", "toggle"}, choice{"Remove a source", "remove"}, choice{"Test all sources", "check"})
		}
		opts = append(opts,
			choice{"Server mode: stdio or HTTP (one central instance)", "server"},
			choice{"How to register with Claude Code / other agents", "register"},
			choice{"Quit", "quit"})

		v, err := w.ui.selectOne("What next?", "Ctrl+C quits here; inside a flow it goes back to this menu", opts, false)
		if err != nil {
			return err
		}
		switch v {
		case "add":
			err = w.addSource()
		case "toggle":
			err = w.toggleSources()
		case "remove":
			err = w.removeSource()
		case "check":
			w.checkAll()
		case "server":
			err = w.serverMode()
		case "register":
			runRegister([]string{"--config", w.file.Path}, w.ui.out, w.stderr)
		case "quit":
			return nil
		}
		if err != nil {
			switch {
			case errors.Is(err, errBack):
				continue
			case errors.Is(err, huh.ErrUserAborted):
				// Ctrl+C inside a sub-flow only cancels that flow.
				w.ui.printf("\n(cancelled, back at the menu)\n")
				continue
			}
			return err
		}
	}
}

// errInputClosed ends the wizard when a piped input is exhausted.
var errInputClosed = errors.New("input ended before the wizard finished")

// errBack returns to the main menu without leaving the wizard.
var errBack = errors.New("back")

func (w *wizard) addSource() error {
	kind, err := w.ui.selectOne("What do you want to read?", "", []choice{
		{"A log file on this machine", "local"},
		{"A log file on a remote host (ssh)", "ssh"},
		{"A Docker container on this machine", "docker-local"},
		{"A Docker container on a remote host (ssh)", "docker-ssh"},
		{"« back", "back"},
	}, false)
	if err != nil {
		return err
	}
	if kind == "back" {
		return errBack
	}

	src := config.Source{}
	var loc config.Source // where to browse
	remote := kind == "ssh" || kind == "docker-ssh"
	if remote {
		loc, err = w.pickHost(kind == "ssh")
		if err != nil {
			return err
		}
	}

	switch kind {
	case "local", "ssh":
		src = loc
		src.Type = config.SourceLocal
		if remote {
			src.Type = config.SourceSSH
		}
		p, err := w.browseFile(loc)
		if err != nil {
			return err
		}
		src.Path = p
	default:
		src = loc
		src.Type = config.SourceDocker
		c, err := w.pickContainer(loc)
		if err != nil {
			return err
		}
		src.Container = c
	}

	if err := w.describe(&src); err != nil {
		return err
	}
	src = configure.Normalize(src)
	if err := src.Validate(); err != nil {
		w.ui.printf("✗ %v\n", err)
		return nil
	}

	w.ui.printf("\nChecking %s ...\n", Describe(src))
	if perr := logs.Probe(expand(src)); perr != nil {
		w.ui.printf("✗ %v\n", perr)
		save, err := w.ui.confirm("Save this source anyway?", false)
		if err != nil {
			return err
		}
		if !save {
			return nil
		}
	} else {
		w.ui.printf("✓ reachable and readable\n")
	}

	if err := w.file.Add(src); err != nil {
		w.ui.printf("✗ %v\n", err)
		return nil
	}
	if err := w.file.Save(); err != nil {
		return err
	}
	w.ui.printf("✓ saved %s to %s\n", src.ID, w.file.Path)
	return nil
}

// pickHost chooses an ssh host from ~/.ssh/config or asks for the details.
// needIdentity is true for `ssh` file sources, which require a key.
func (w *wizard) pickHost(needIdentity bool) (config.Source, error) {
	loc := config.Source{}
	if aliases := configure.ListSSHAliases(); len(aliases) > 0 {
		opts := make([]choice, 0, len(aliases)+1)
		for _, a := range aliases {
			opts = append(opts, choice{a, "alias:" + a})
		}
		opts = append(opts, choice{"« enter host details manually", "manual"}, choice{"« back to menu", "back"})
		v, err := w.ui.selectOne("Which host? (from ~/.ssh/config)", "just type to filter, enter to choose", opts, true)
		if err != nil {
			return loc, err
		}
		if v == "back" {
			return loc, errBack
		}
		if alias, ok := strings.CutPrefix(v, "alias:"); ok {
			h, err := configure.ResolveSSHHost(alias)
			if err != nil {
				w.ui.printf("✗ could not resolve %s with `ssh -G`: %v\n", alias, err)
			} else {
				loc = config.Source{Host: alias, User: h.User, IdentityFile: h.IdentityFile, Port: h.Port}
				w.ui.printf("→ %s@%s:%d  key: %s\n", h.User, h.HostName, h.Port, orNone(h.IdentityFile))
			}
		}
	}

	if loc.Host == "" {
		host, err := w.ui.input("Host (name or IP)", "", required("host"))
		if err != nil {
			return loc, err
		}
		def := ""
		if u, err := user.Current(); err == nil {
			def = u.Username
		}
		usr, err := w.ui.input("User", def, required("user"))
		if err != nil {
			return loc, err
		}
		portS, err := w.ui.input("Port", "22", func(s string) error {
			if n, err := strconv.Atoi(strings.TrimSpace(s)); err != nil || n < 1 || n > 65535 {
				return errors.New("port must be 1-65535")
			}
			return nil
		})
		if err != nil {
			return loc, err
		}
		port, _ := strconv.Atoi(portS)
		loc = config.Source{Host: host, User: usr, Port: port}
	}

	if loc.IdentityFile == "" {
		def := ""
		if home, err := os.UserHomeDir(); err == nil {
			if _, err := os.Stat(filepath.Join(home, ".ssh", "id_ed25519")); err == nil {
				def = "~/.ssh/id_ed25519"
			}
		}
		var v func(string) error
		title := "Private key file (empty = let ssh/ssh-agent decide)"
		if needIdentity {
			v, title = required("key file"), "Private key file"
		}
		key, err := w.ui.input(title, def, v)
		if err != nil {
			return loc, err
		}
		loc.IdentityFile = key
	}
	if loc.Port == 0 && needIdentity {
		loc.Port = 22
	}
	return loc, nil
}

func orNone(s string) string {
	if s == "" {
		return "(none found)"
	}
	return s
}

func (w *wizard) where(loc config.Source) string {
	if loc.Host != "" {
		return loc.Host
	}
	return "this machine"
}

// EnvBrowseDir overrides where the local file browser starts.
const EnvBrowseDir = "MCPLOGVIEW_BROWSE_DIR"

// browseStartDir is where file browsing begins: $MCPLOGVIEW_BROWSE_DIR (local
// only), else /var/log, else the home directory.
var browseStartDir = func(remote bool) string {
	if !remote {
		if v := os.Getenv(EnvBrowseDir); v != "" {
			return v
		}
		if st, err := os.Stat("/var/log"); err != nil || !st.IsDir() {
			home, _ := os.UserHomeDir()
			return home
		}
	}
	return "/var/log"
}

// browseFile lets the user walk the file system of loc (local or over ssh).
func (w *wizard) browseFile(loc config.Source) (string, error) {
	remote := loc.Host != ""
	join := filepath.Join
	parent := filepath.Dir
	dir := browseStartDir(remote)
	if remote {
		join, parent = path.Join, logs.ParentDir
	}

	for {
		entries, err := logs.ListDir(loc, dir)
		if err != nil {
			w.ui.printf("✗ cannot list %s on %s: %v\n", dir, w.where(loc), err)
		}
		opts := []choice{{"[..] parent directory", "up"}}
		for _, e := range entries {
			if e.IsDir {
				opts = append(opts, choice{e.Name + "/", "dir:" + e.Name})
			} else {
				opts = append(opts, choice{e.Name, "file:" + e.Name})
			}
		}
		opts = append(opts, choice{"[type a path manually]", "manual"}, choice{"« back to menu", "cancel"})

		v, err := w.ui.selectOne(fmt.Sprintf("%s: %s", w.where(loc), dir), "just type to filter; enter opens a folder or picks a file", opts, true)
		if err != nil {
			return "", err
		}
		switch {
		case v == "up":
			dir = parent(dir)
		case v == "cancel":
			return "", errBack
		case v == "manual":
			p, err := w.ui.input("Path of the log file", join(dir, ""), required("path"))
			if err != nil {
				return "", err
			}
			return p, nil
		case strings.HasPrefix(v, "dir:"):
			dir = join(dir, strings.TrimPrefix(v, "dir:"))
		case strings.HasPrefix(v, "file:"):
			return join(dir, strings.TrimPrefix(v, "file:")), nil
		}
	}
}

func (w *wizard) pickContainer(loc config.Source) (string, error) {
	cs, err := logs.ListContainers(loc)
	if err != nil {
		w.ui.printf("✗ cannot list containers on %s: %v\n", w.where(loc), err)
	}
	opts := make([]choice, 0, len(cs)+2)
	for _, c := range cs {
		opts = append(opts, choice{fmt.Sprintf("%-32s %-28s %s", c.Name, trunc(c.Image, 28), c.Status), "c:" + c.Name})
	}
	opts = append(opts, choice{"[type a container name]", "manual"}, choice{"« back to menu", "cancel"})

	v, err := w.ui.selectOne("Container on "+w.where(loc), "just type to filter (name, image, status); enter to choose", opts, true)
	if err != nil {
		return "", err
	}
	switch {
	case v == "cancel":
		return "", errBack
	case v == "manual":
		return w.ui.input("Container name", "", func(s string) error {
			if !idPattern.MatchString(strings.TrimSpace(s)) {
				return errors.New("letters, digits, '_', '.', '-' only")
			}
			return nil
		})
	}
	return strings.TrimPrefix(v, "c:"), nil
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// describe asks for id, label, description and hint.
func (w *wizard) describe(src *config.Source) error {
	def := w.uniqueID(slug(defaultName(*src)))
	id, err := w.ui.input("Source id (used by agents, e.g. "+def+")", def, func(s string) error {
		s = strings.TrimSpace(s)
		switch {
		case !idPattern.MatchString(s):
			return errors.New("letters, digits, '_', '.', '-' only")
		case w.file.Find(s) >= 0:
			return fmt.Errorf("id %q is already used", s)
		}
		return nil
	})
	if err != nil {
		return err
	}
	src.ID = id
	if src.Label, err = w.ui.input("Label (human readable name)", id, required("label")); err != nil {
		return err
	}
	if src.Description, err = w.ui.input("Description (optional: what is this log?)", "", nil); err != nil {
		return err
	}
	src.Hint, err = w.ui.input("Hint for the agent (optional: where to start?)", "", nil)
	return err
}

func defaultName(s config.Source) string {
	if s.Type == config.SourceDocker {
		return s.Container
	}
	base := filepath.Base(s.Path)
	if ext := filepath.Ext(base); ext != "" && len(base) > len(ext) {
		base = strings.TrimSuffix(base, ext)
	}
	return base
}

var slugRe = regexp.MustCompile(`[^a-z0-9_.-]+`)

func slug(s string) string {
	s = strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-.")
	if s == "" {
		return "log"
	}
	return s
}

func (w *wizard) uniqueID(base string) string {
	id := base
	for i := 2; w.file.Find(id) >= 0; i++ {
		id = fmt.Sprintf("%s-%d", base, i)
	}
	return id
}

func (w *wizard) removeSource() error {
	opts := make([]choice, 0, len(w.file.Sources)+1)
	for _, s := range w.file.Sources {
		opts = append(opts, choice{fmt.Sprintf("%-18s %s", s.ID, Describe(s)), s.ID})
	}
	opts = append(opts, choice{"« back", "back"})
	id, err := w.ui.selectOne("Remove which source?", "", opts, false)
	if err != nil || id == "back" {
		if err == nil {
			err = errBack
		}
		return err
	}
	ok, err := w.ui.confirm(fmt.Sprintf("Remove %q from %s?", id, w.file.Path), false)
	if err != nil || !ok {
		return err
	}
	if err := w.file.Remove(id); err != nil {
		return err
	}
	if err := w.file.Save(); err != nil {
		return err
	}
	w.ui.printf("✓ removed %s\n", id)
	return nil
}

func (w *wizard) checkAll() {
	sources := make([]config.Source, 0, len(w.file.Sources))
	for _, s := range w.file.Sources {
		sources = append(sources, expand(s))
	}
	w.ui.printf("Testing %d source(s) ...\n", len(sources))
	for _, r := range checkSources(sources) {
		if r.err != nil {
			w.ui.printf("✗ %-18s %s\n    %v\n", r.src.ID, Describe(r.src), r.err)
		} else {
			w.ui.printf("✓ %-18s %s\n", r.src.ID, Describe(r.src))
		}
	}
}

// newToggleField builds the on/off multi-select for the given sources and
// returns it with the slice that receives the ids that stay ON.
func newToggleField(sources []config.Source) (*huh.MultiSelect[string], *[]string) {
	opts := make([]huh.Option[string], 0, len(sources))
	picked := []string{}
	for _, s := range sources {
		opts = append(opts, huh.NewOption(fmt.Sprintf("%-18s %-7s %s", s.ID, s.Type, Describe(s)), s.ID))
		if !s.Disabled {
			picked = append(picked, s.ID)
		}
	}
	// Without an explicit height the option list collapses to nothing (the
	// user sees an empty box), so size it to the content: the title and
	// description lines plus one row per source, capped for long lists.
	height := len(opts) + 4
	if height > 20 {
		height = 20
	}
	ms := huh.NewMultiSelect[string]().
		Title("Which sources should be ON?").
		Description("space (or x) toggles, enter confirms. Off sources stay in the file but agents cannot see them.").
		Options(opts...).Value(&picked).Height(height)
	return ms, &picked
}

// toggleSources lets the user pick which configured sources are switched on.
func (w *wizard) toggleSources() error {
	ms, pickedPtr := newToggleField(w.file.Sources)
	if err := w.ui.run(ms); err != nil {
		return err
	}

	on := map[string]bool{}
	for _, id := range *pickedPtr {
		on[id] = true
	}
	changed := 0
	for _, s := range w.file.Sources {
		if s.Disabled == !on[s.ID] {
			continue
		}
		_ = w.file.SetDisabled(s.ID, !on[s.ID])
		changed++
	}
	if changed == 0 {
		w.ui.printf("(no changes)\n")
		return nil
	}
	if err := w.file.Save(); err != nil {
		return err
	}
	w.ui.printf("✓ saved. A running mcplogview picks this up within a few seconds.\n")
	return nil
}

// serverMode configures how the server is reached: stdio (an agent starts its
// own process) or HTTP (one central instance that agents connect to).
func (w *wizard) serverMode() error {
	cur, err := w.file.Transport()
	if err != nil {
		return err
	}
	w.ui.printf("\nCurrent: ")
	printTransport(w.ui.out, cur, false)

	mode, err := w.ui.selectOne("How should agents connect?", "", []choice{
		{"stdio: each agent starts its own mcplogview (no network port)", config.TransportStdio},
		{"HTTP: one central instance that agents connect to", config.TransportHTTP},
		{"HTTPS with a self-signed certificate (development only)", config.TransportHTTPSDev},
		{"« back to menu", "back"},
	}, false)
	if err != nil {
		return err
	}
	if mode == "back" {
		return errBack
	}

	change := transportChange{mode: mode}
	if mode != config.TransportStdio {
		where, err := w.ui.selectOne("Who may connect?", "", []choice{
			{"This machine only (127.0.0.1)", "127.0.0.1"},
			{"Other machines too (all network interfaces, 0.0.0.0)", "0.0.0.0"},
			{"A specific address…", "custom"},
		}, false)
		if err != nil {
			return err
		}
		if where == "custom" {
			if where, err = w.ui.input("Listen address", cur.Host, required("address")); err != nil {
				return err
			}
		}
		change.host = where

		portS, err := w.ui.input("Port", strconv.Itoa(cur.Port), func(s string) error {
			if n, err := strconv.Atoi(strings.TrimSpace(s)); err != nil || n < 1 || n > 65535 {
				return errors.New("port must be 1-65535")
			}
			return nil
		})
		if err != nil {
			return err
		}
		change.port, _ = strconv.Atoi(portS)

		if change.path, err = w.ui.input("URL path", cur.Path, required("path")); err != nil {
			return err
		}

		tok, err := w.chooseToken(cur, where)
		if err != nil {
			return err
		}
		change.newToken = tok == "new"
		change.noToken = tok == "none"
		change.allowNoAuth = tok == "none"
	}

	next, generated, err := applyTransport(w.file, change)
	if err != nil {
		w.ui.printf("✗ %v\n", err)
		return nil
	}
	w.ui.printf("\n✓ saved to %s\n", w.file.Path)
	printTransport(w.ui.out, next, generated)
	w.ui.printf("\nA running server needs a restart for this to apply (the token is picked up live).\n\n")

	reg, rerr := currentRegistration(w.file.Path)
	if rerr == nil {
		reg.transport, reg.token = next, config.ResolveToken(next.AuthToken, os.Getenv)
		reg.print(w.ui.out)
	}
	return nil
}

// chooseToken decides what happens with the auth token: "keep", "new" or "none".
func (w *wizard) chooseToken(cur config.Transport, listenHost string) (string, error) {
	exposed := !config.Transport{Host: listenHost}.IsLoopback()
	opts := []choice{}
	if cur.AuthToken != "" {
		opts = append(opts, choice{"Keep the current token", "keep"})
	}
	newLabel := "Generate a token (recommended)"
	if cur.AuthToken != "" {
		newLabel = "Generate a new token (replaces the old one)"
	}
	opts = append(opts, choice{newLabel, "new"}, choice{"No token (anyone who can reach the port can read the logs)", "none"})

	desc := "Clients send it as 'Authorization: Bearer <token>'."
	if exposed {
		desc += " Plain HTTP is unencrypted: use a trusted network, an SSH tunnel or a TLS proxy."
	}
	v, err := w.ui.selectOne("Protect the HTTP endpoint with a token?", desc, opts, false)
	if err != nil {
		return "", err
	}
	if v == "none" && exposed {
		ok, err := w.ui.confirm("Really expose the logs on the network without a token?", false)
		if err != nil {
			return "", err
		}
		if !ok {
			return w.chooseToken(cur, listenHost)
		}
	}
	return v, nil
}
