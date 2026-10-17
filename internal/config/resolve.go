package config

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	// EnvConfig names the environment variable that points to the config file.
	EnvConfig = "LOG_SOURCES_CONFIG"
	// LocalConfigName is looked up in the current working directory.
	LocalConfigName = "mcplogview.json"
)

// Origin describes where the effective configuration came from.
type Origin string

const (
	OriginFlag    Origin = "--config flag"
	OriginEnv     Origin = EnvConfig + " env"
	OriginCwd     Origin = "current directory"
	OriginUser    Origin = "user config"
	OriginDefault Origin = "built-in defaults"
)

// UserConfigPath is the second fallback: ~/.config/mcplogview/config.json
// (XDG_CONFIG_HOME is honoured).
func UserConfigPath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "mcplogview", "config.json")
}

// StateDir holds the default self-log (XDG_STATE_HOME or ~/.local/state).
func StateDir() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return filepath.Join(os.TempDir(), "mcplogview")
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "mcplogview")
}

// Resolve picks the config file. Order:
//
//  1. --config flag (must exist)
//  2. LOG_SOURCES_CONFIG env (must exist)
//  3. ./mcplogview.json in the current directory
//  4. ~/.config/mcplogview/config.json
//  5. built-in defaults (empty path)
func Resolve(flagPath string, getenv func(string) string, cwd string) (path string, origin Origin, err error) {
	if flagPath != "" {
		if _, statErr := os.Stat(flagPath); statErr != nil {
			return "", "", fmt.Errorf("config file from --config not found: %w", statErr)
		}
		return flagPath, OriginFlag, nil
	}
	if env := getenv(EnvConfig); env != "" {
		if _, statErr := os.Stat(env); statErr != nil {
			return "", "", fmt.Errorf("config file from %s not found: %w", EnvConfig, statErr)
		}
		return env, OriginEnv, nil
	}
	local := filepath.Join(cwd, LocalConfigName)
	if isFile(local) {
		return local, OriginCwd, nil
	}
	if user := UserConfigPath(); user != "" && isFile(user) {
		return user, OriginUser, nil
	}
	return "", OriginDefault, nil
}

// LoadEffective resolves and loads the configuration, falling back to the
// built-in defaults when no file exists.
func LoadEffective(flagPath string, getenv func(string) string, cwd string) (Config, string, Origin, error) {
	path, origin, err := Resolve(flagPath, getenv, cwd)
	if err != nil {
		return Config{}, "", "", err
	}
	if path == "" {
		cfg := Defaults()
		if err := cfg.Finalize(cwd); err != nil {
			return Config{}, "", "", err
		}
		return cfg, "", origin, nil
	}
	cfg, err := Load(path)
	return cfg, path, origin, err
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
