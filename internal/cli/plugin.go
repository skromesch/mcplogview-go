package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"text/tabwriter"

	"github.com/skromesch/mcplogview-go/internal/config"
	"github.com/skromesch/mcplogview-go/internal/plugin"
	"github.com/skromesch/mcplogview-go/internal/pluginmgr"
	"github.com/skromesch/mcplogview-go/internal/server"
)

const pluginUsage = `Usage:
  mcplogview plugin sync [--update] [NAME...]  clone/fetch plugins with "git" and check out the locked commit
                                               (--update: move to the latest of "ref" and re-lock)
  mcplogview plugin list                       show plugins, their install state and whether they load
  mcplogview plugin remove NAME                delete a plugin installed by sync (the config is not edited)
  mcplogview plugin test [-run REGEXP] DIR     run DIR/tests/*_test.lua against the plugin in DIR
Common flags: --config FILE
`

func runPlugin(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, pluginUsage)
		return 2
	}
	sub, rest := args[0], args[1:]
	fs, configPath := newFlags("plugin "+sub, stderr)
	update := false
	runPattern := ""
	if sub == "test" {
		fs.StringVar(&runPattern, "run", "", "only run cases whose name or file matches this regexp")
	}
	if sub == "sync" {
		fs.BoolVar(&update, "update", false, "move plugins to the latest commit of their ref")
	}
	switch sub {
	case "sync", "list", "ls", "remove", "rm", "test":
	default:
		fmt.Fprintf(stderr, "mcplogview: unknown plugin command %q\n\n%s", sub, pluginUsage)
		return 2
	}
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if (sub == "remove" || sub == "rm") && fs.NArg() != 1 {
		fmt.Fprint(stderr, pluginUsage)
		return 2
	}
	if sub == "test" {
		args := fs.Args()
		if len(args) > 1 { // flags after the directory: test DIR -run X
			if err := fs.Parse(args[1:]); err != nil {
				return 2
			}
			args = append(args[:1:1], fs.Args()...)
		}
		return pluginTest(args, runPattern, stdout, stderr)
	}

	m, err := newPluginManager(*configPath, stdout)
	if err != nil {
		fmt.Fprintln(stderr, "mcplogview:", err)
		return 1
	}

	switch sub {
	case "sync":
		err = m.Sync(context.Background(), fs.Args(), update)
	case "remove", "rm":
		err = m.Remove(fs.Arg(0))
	default:
		err = pluginList(m, stdout)
	}
	if err != nil {
		fmt.Fprintln(stderr, "mcplogview:", err)
		return 1
	}
	return 0
}

// newPluginManager loads the effective config (configPath may be empty) and
// returns a plugin manager whose lock file sits next to that config.
func newPluginManager(configPath string, out io.Writer) (*pluginmgr.Manager, error) {
	cwd, _ := os.Getwd()
	cfg, path, _, err := config.LoadEffective(configPath, os.Getenv, cwd)
	if err != nil {
		return nil, err
	}
	base := cwd
	if path != "" {
		base = filepath.Dir(absPath(path))
	}
	return &pluginmgr.Manager{Config: cfg, LockPath: filepath.Join(base, pluginmgr.LockFile), Out: out}, nil
}

// pluginTest runs a plugin's Lua tests; exit 1 on any failure.
func pluginTest(args []string, pattern string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprint(stderr, pluginUsage)
		return 2
	}
	opts := plugin.TestOptions{Out: stdout, ReservedToolNames: server.ToolNames}
	if pattern != "" {
		re, err := regexp.Compile(pattern)
		if err != nil {
			fmt.Fprintln(stderr, "mcplogview: -run:", err)
			return 2
		}
		opts.Run = re
	}
	res, err := plugin.RunTests(context.Background(), args[0], opts)
	if err != nil {
		fmt.Fprintln(stderr, "mcplogview:", err)
		return 1
	}
	if res.Failed > 0 {
		return 1
	}
	return 0
}

func pluginList(m *pluginmgr.Manager, stdout io.Writer) error {
	rows, err := m.List()
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Fprintln(stdout, "no plugins configured")
		return nil
	}
	// load the plugins like the server does
	h := plugin.NewHost(plugin.Options{ReservedToolNames: server.ToolNames})
	h.Reload(m.Config)
	load := map[string]string{}
	for _, s := range h.Status() {
		if s.OK {
			load[s.Name] = "load OK"
		} else {
			load[s.Name] = "load error: " + s.Error
		}
	}
	h.Close()

	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tENABLED\tGIT\tLOCKED\tINSTALLED\tSTATE\tLOAD")
	for _, r := range rows {
		src := "-"
		if r.Git != "" {
			src = r.Git
			if r.Ref != "" {
				src += "@" + r.Ref
			}
		}
		l, ok := load[r.Name]
		if !ok {
			l = "-"
		}
		fmt.Fprintf(tw, "%s\t%v\t%s\t%s\t%s\t%s\t%s\n", r.Name, r.Enabled, src, dash(shortSHA(r.Locked)), dash(shortSHA(r.Installed)), r.State, l)
	}
	return tw.Flush()
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
