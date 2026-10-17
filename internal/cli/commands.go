package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"text/tabwriter"

	"github.com/skromesch/mcplogview-go/internal/config"
	"github.com/skromesch/mcplogview-go/internal/configure"
	"github.com/skromesch/mcplogview-go/internal/logs"
)

const exampleConfig = `{
  "transport": { "mode": "stdio" },
  "logging": { "enabled": true },
  "sources": [
    {
      "type": "local",
      "id": "app",
      "label": "Application Log",
      "path": "/var/log/myapp/app.log",
      "description": "Main backend service log",
      "hint": "Start here for backend API runtime errors."
    },
    {
      "type": "ssh",
      "id": "prod-api",
      "label": "Prod API Log",
      "host": "10.0.0.12",
      "port": 22,
      "user": "ubuntu",
      "identityFile": "~/.ssh/id_ed25519",
      "path": "/var/log/myapp/api.log"
    },
    {
      "type": "docker",
      "id": "api-container",
      "label": "API container",
      "container": "my-api"
    }
  ]
}
`

// targetPath is the file edit commands write to: --config, then $LOG_SOURCES_CONFIG,
// then an existing ./mcplogview.json or user config, else a new ./mcplogview.json.
func targetPath(flagPath string) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	path, _, err := config.Resolve(flagPath, os.Getenv, cwd)
	if err != nil {
		// an explicit --config that does not exist yet is fine for editing
		if flagPath != "" {
			return absPath(flagPath), nil
		}
		return "", err
	}
	if path == "" {
		return filepath.Join(cwd, config.LocalConfigName), nil
	}
	return absPath(path), nil
}

func absPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

func runInit(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	force := fs.Bool("force", false, "overwrite an existing file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if _, err := os.Stat(config.LocalConfigName); err == nil && !*force {
		fmt.Fprintf(stderr, "mcplogview: %s already exists (use --force to overwrite)\n", config.LocalConfigName)
		return 1
	}
	if err := os.WriteFile(config.LocalConfigName, []byte(exampleConfig), 0o644); err != nil {
		fmt.Fprintln(stderr, "mcplogview:", err)
		return 1
	}
	fmt.Fprintf(stderr, "mcplogview: wrote %s\n", config.LocalConfigName)
	return 0
}

func runWhere(args []string, stdout, stderr io.Writer) int {
	fs, configPath := newFlags("where", stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cwd, _ := os.Getwd()
	path, origin, err := config.Resolve(*configPath, os.Getenv, cwd)
	if err != nil {
		fmt.Fprintln(stderr, "mcplogview:", err)
		return 1
	}
	if path == "" {
		fmt.Fprintf(stdout, "(none) %s\n", origin)
		return 0
	}
	fmt.Fprintf(stdout, "%s (%s)\n", path, origin)
	return 0
}

// Describe renders where a source points to, for listings.
func Describe(s config.Source) string {
	switch s.Type {
	case config.SourceSSH:
		return fmt.Sprintf("%s@%s:%s", s.User, s.Host, s.Path)
	case config.SourceDocker:
		if s.Host != "" {
			if s.User != "" {
				return fmt.Sprintf("%s @ %s@%s", s.Container, s.User, s.Host)
			}
			return fmt.Sprintf("%s @ %s", s.Container, s.Host)
		}
		return s.Container
	}
	return s.Path
}

func runList(args []string, stdout, stderr io.Writer) int {
	fs, configPath := newFlags("list", stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cwd, _ := os.Getwd()
	cfg, path, origin, err := config.LoadEffective(*configPath, os.Getenv, cwd)
	if err != nil {
		fmt.Fprintln(stderr, "mcplogview:", err)
		return 1
	}
	if path == "" {
		fmt.Fprintf(stderr, "config: none (%s)\n", origin)
	} else {
		fmt.Fprintf(stderr, "config: %s (%s)\n", path, origin)
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATE\tTYPE\tLABEL\tLOCATION")
	for _, s := range cfg.Sources {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", s.ID, stateOf(s), s.Type, s.Label, Describe(s))
	}
	tw.Flush()
	return 0
}

type sourceFlags struct {
	id, label, description, hint string
	path, container              string
	host, user, identityFile     string
	port                         int
	strict, noCheck              bool
}

func runAdd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(stderr, "usage: mcplogview add local|ssh|docker --id ID ...  (see `mcplogview help`)")
		return 2
	}
	kind, rest := args[0], args[1:]
	if kind != config.SourceLocal && kind != config.SourceSSH && kind != config.SourceDocker {
		fmt.Fprintf(stderr, "mcplogview: unknown source type %q (local, ssh, docker)\n", kind)
		return 2
	}

	fs, configPath := newFlags("add "+kind, stderr)
	var f sourceFlags
	fs.StringVar(&f.id, "id", "", "unique source id (required)")
	fs.StringVar(&f.label, "label", "", "human readable name (default: id)")
	fs.StringVar(&f.description, "description", "", "what this log is")
	fs.StringVar(&f.hint, "hint", "", "tip shown to the agent")
	fs.BoolVar(&f.noCheck, "no-check", false, "save without testing the source first")
	switch kind {
	case config.SourceLocal:
		fs.StringVar(&f.path, "path", "", "log file path (required)")
	case config.SourceSSH:
		fs.StringVar(&f.path, "path", "", "remote log file path (required)")
	case config.SourceDocker:
		fs.StringVar(&f.container, "container", "", "container name (required)")
	}
	if kind != config.SourceLocal {
		fs.StringVar(&f.host, "host", "", "ssh host or ~/.ssh/config alias")
		fs.StringVar(&f.user, "user", "", "ssh user")
		fs.StringVar(&f.identityFile, "identity-file", "", "ssh private key")
		fs.IntVar(&f.port, "port", 0, "ssh port (ssh sources default to 22)")
		fs.BoolVar(&f.strict, "strict-host-key-checking", false, "verify the host key")
	}
	if err := fs.Parse(rest); err != nil {
		return 2
	}

	src := config.Source{
		Type: kind, ID: f.id, Label: f.label, Description: f.description, Hint: f.hint,
		Path: f.path, Container: f.container, Host: f.host, User: f.user,
		IdentityFile: f.identityFile, Port: f.port, StrictHostKeyChecking: f.strict,
	}
	if src.Label == "" {
		src.Label = src.ID
	}
	if kind == config.SourceLocal && src.Path != "" {
		src.Path = absPath(src.Path)
	}
	src = configure.Normalize(src)
	if err := src.Validate(); err != nil {
		fmt.Fprintln(stderr, "mcplogview:", err)
		return 2
	}

	target, err := targetPath(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, "mcplogview:", err)
		return 1
	}
	file, err := configure.Open(target)
	if err != nil {
		fmt.Fprintln(stderr, "mcplogview:", err)
		return 1
	}
	if file.Find(src.ID) >= 0 {
		fmt.Fprintf(stderr, "mcplogview: source %q already exists in %s (remove it first)\n", src.ID, target)
		return 1
	}

	if !f.noCheck {
		if err := logs.Probe(expand(src)); err != nil {
			fmt.Fprintf(stderr, "mcplogview: check failed: %v\n(use --no-check to save anyway)\n", err)
			return 1
		}
		fmt.Fprintln(stderr, "check: ok")
	}
	if err := file.Add(src); err != nil {
		fmt.Fprintln(stderr, "mcplogview:", err)
		return 1
	}
	if err := file.Save(); err != nil {
		fmt.Fprintln(stderr, "mcplogview:", err)
		return 1
	}
	fmt.Fprintf(stdout, "added %s (%s) to %s\n", src.ID, Describe(src), target)
	return 0
}

// expand resolves "~" in a source the way config.Load does, for probing.
func expand(s config.Source) config.Source {
	cfg := config.Config{Sources: []config.Source{s}}
	cfg.Logging.Enabled = false
	c := config.Defaults()
	c.Logging.Enabled = false
	c.Sources = cfg.Sources
	if err := c.Finalize(mustGetwd()); err == nil && len(c.Sources) == 1 {
		return c.Sources[0]
	}
	return s
}

func mustGetwd() string {
	wd, _ := os.Getwd()
	return wd
}

func runRemove(args []string, stdout, stderr io.Writer) int {
	fs, configPath := newFlags("remove", stderr)
	// allow the id before or after flags
	var ids []string
	rest := args
	for len(rest) > 0 {
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if fs.NArg() == 0 {
			break
		}
		ids = append(ids, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	if len(ids) != 1 {
		fmt.Fprintln(stderr, "usage: mcplogview remove ID [--config FILE]")
		return 2
	}
	target, err := targetPath(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, "mcplogview:", err)
		return 1
	}
	file, err := configure.Open(target)
	if err != nil {
		fmt.Fprintln(stderr, "mcplogview:", err)
		return 1
	}
	if err := file.Remove(ids[0]); err != nil {
		fmt.Fprintf(stderr, "mcplogview: %v in %s\n", err, target)
		return 1
	}
	if err := file.Save(); err != nil {
		fmt.Fprintln(stderr, "mcplogview:", err)
		return 1
	}
	fmt.Fprintf(stdout, "removed %s from %s\n", ids[0], target)
	return 0
}

type checkResult struct {
	src config.Source
	err error
}

// checkSources probes sources in parallel and returns results in input order.
func checkSources(sources []config.Source) []checkResult {
	res := make([]checkResult, len(sources))
	var wg sync.WaitGroup
	for i, s := range sources {
		wg.Add(1)
		go func(i int, s config.Source) {
			defer wg.Done()
			res[i] = checkResult{src: s, err: logs.Probe(s)}
		}(i, s)
	}
	wg.Wait()
	return res
}

func runCheck(args []string, stdout, stderr io.Writer) int {
	fs, configPath := newFlags("check", stderr)
	var ids []string
	rest := args
	for len(rest) > 0 {
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if fs.NArg() == 0 {
			break
		}
		ids = append(ids, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	cwd, _ := os.Getwd()
	cfg, _, _, err := config.LoadEffective(*configPath, os.Getenv, cwd)
	if err != nil {
		fmt.Fprintln(stderr, "mcplogview:", err)
		return 1
	}

	var sources []config.Source
	for _, s := range cfg.Sources {
		if len(ids) > 0 && !contains(ids, s.ID) {
			continue
		}
		if len(ids) == 0 && s.ID == cfg.Logging.SelfSourceID {
			continue // created by the server on first start
		}
		if len(ids) == 0 && s.Disabled {
			continue // switched off; name it explicitly to test it anyway
		}
		sources = append(sources, s)
	}
	for _, id := range ids {
		found := false
		for _, s := range sources {
			found = found || s.ID == id
		}
		if !found {
			fmt.Fprintf(stderr, "mcplogview: no source with id %q\n", id)
			return 1
		}
	}
	if len(sources) == 0 {
		fmt.Fprintln(stdout, "no sources to check")
		return 0
	}

	failed := 0
	for _, r := range checkSources(sources) {
		if r.err != nil {
			failed++
			fmt.Fprintf(stdout, "FAIL  %-20s %s\n      %v\n", r.src.ID, Describe(r.src), r.err)
		} else {
			fmt.Fprintf(stdout, "ok    %-20s %s\n", r.src.ID, Describe(r.src))
		}
	}
	if failed > 0 {
		return 1
	}
	return 0
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func stateOf(s config.Source) string {
	if s.Disabled {
		return "off"
	}
	return "on"
}

// runToggle implements `enable` / `disable`.
func runToggle(args []string, disable bool, stdout, stderr io.Writer) int {
	verb := "enable"
	if disable {
		verb = "disable"
	}
	fs, configPath := newFlags(verb, stderr)
	all := fs.Bool("all", false, verb+" every source")
	var ids []string
	rest := args
	for len(rest) > 0 {
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if fs.NArg() == 0 {
			break
		}
		ids = append(ids, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	if len(ids) == 0 && !*all {
		fmt.Fprintf(stderr, "usage: mcplogview %s ID... | --all  [--config FILE]\n", verb)
		return 2
	}

	target, err := targetPath(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, "mcplogview:", err)
		return 1
	}
	file, err := configure.Open(target)
	if err != nil {
		fmt.Fprintln(stderr, "mcplogview:", err)
		return 1
	}
	if *all {
		for _, s := range file.Sources {
			ids = append(ids, s.ID)
		}
	}
	for _, id := range ids {
		if err := file.SetDisabled(id, disable); err != nil {
			fmt.Fprintf(stderr, "mcplogview: %v in %s\n", err, target)
			return 1
		}
	}
	if err := file.Save(); err != nil {
		fmt.Fprintln(stderr, "mcplogview:", err)
		return 1
	}
	fmt.Fprintf(stdout, "%sd %s in %s\n", verb, strings.Join(ids, ", "), target)
	return 0
}
