package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/skromesch/mcplogview-go/internal/buildinfo"
	"github.com/skromesch/mcplogview-go/internal/config"
	"github.com/skromesch/mcplogview-go/internal/server"
	"github.com/skromesch/mcplogview-go/internal/serverlog"
)

func newFlags(name string, stderr io.Writer) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs, fs.String("config", "", "path to config JSON")
}

func runServe(args []string, stderr io.Writer) int {
	fs, configPath := newFlags("serve", stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(stderr, "mcplogview:", err)
		return 1
	}
	cfg, path, origin, err := config.LoadEffective(*configPath, os.Getenv, cwd)
	if err != nil {
		fmt.Fprintln(stderr, "mcplogview:", err)
		return 1
	}
	if path == "" {
		fmt.Fprintf(stderr, "mcplogview: no config found (%s, ~/.config/mcplogview/config.json); using %s\n", filepath.Join(cwd, config.LocalConfigName), origin)
	} else {
		fmt.Fprintf(stderr, "mcplogview: config %s (%s)\n", path, origin)
	}

	logger := serverlog.Noop()
	if cfg.Logging.Enabled {
		l, err := serverlog.New(cfg.Logging.Path, cfg.Logging.MaxFileSize, cfg.Logging.MaxBackups)
		if err != nil {
			fmt.Fprintln(stderr, "mcplogview: cannot create self-log:", err)
			return 1
		}
		logger = l
		fmt.Fprintf(stderr, "mcplogview: log file %s\n", cfg.Logging.Path)
	}
	logger.Info("build " + buildinfo.String())

	rt := server.NewRuntime(cfg, logger)
	srv := server.New(rt)
	logger.Info(fmt.Sprintf("server initialized with %d sources (%d enabled)", len(cfg.Sources), len(cfg.EnabledSources())))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if path != "" {
		go server.WatchConfig(ctx, rt, path, 2*time.Second)
	}

	if cfg.Transport.Mode == config.TransportStdio {
		err = server.RunStdio(ctx, srv, logger, cfg.Logging.Debug, cfg.Logging.MaxBodyChars)
	} else {
		tokenFn := func() string {
			return config.ResolveToken(rt.Config().Transport.AuthToken, os.Getenv)
		}
		err = server.RunHTTP(ctx, srv, cfg, logger, tokenFn)
	}

	logger.Info("shutdown initiated")
	rt.Close()
	logger.Close()
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
		logger.Error("server stopped with error", err)
		fmt.Fprintln(stderr, "mcplogview:", err)
		return 1
	}
	return 0
}
