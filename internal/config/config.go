// Package config loads and validates the mcplogview JSON configuration.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// containerName matches Docker container names and IDs. It is deliberately
// strict because the value ends up inside a (possibly remote) shell command.
var containerName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

const (
	SourceLocal  = "local"
	SourceSSH    = "ssh"
	SourceDocker = "docker"

	TransportStdio    = "stdio"
	TransportHTTP     = "http"
	TransportHTTPSDev = "https-dev"
)

type Transport struct {
	Mode      string `json:"mode"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	Path      string `json:"path"`
	HTTPSKey  string `json:"httpsKey,omitempty"`
	HTTPSCert string `json:"httpsCert,omitempty"`
	// AuthToken, when set, is required as "Authorization: Bearer <token>" on
	// every HTTP request. $MCPLOGVIEW_TOKEN overrides it.
	AuthToken string `json:"authToken,omitempty"`
	// AllowedOrigins are extra browser origins (e.g. "https://inspector.example")
	// allowed to call the HTTP endpoint without a token; "*" allows any.
	AllowedOrigins []string `json:"allowedOrigins,omitempty"`
	// AllowNoAuth lets the server listen on a non-loopback address without a token.
	AllowNoAuth bool `json:"allowNoAuth,omitempty"`
}

// EnvToken overrides transport.authToken when set.
const EnvToken = "MCPLOGVIEW_TOKEN"

// IsLoopback reports whether the listen host only accepts local connections.
func (t Transport) IsLoopback() bool {
	h := strings.Trim(t.Host, "[]")
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// ResolveToken returns the effective HTTP auth token (env wins over config).
func ResolveToken(configToken string, getenv func(string) string) string {
	if v := getenv(EnvToken); v != "" {
		return v
	}
	return configToken
}

type Logging struct {
	Enabled         bool   `json:"enabled"`
	Debug           bool   `json:"debug"`
	MaxBodyChars    int    `json:"maxBodyChars"`
	Path            string `json:"path"`
	SelfSourceID    string `json:"selfSourceId"`
	SelfSourceLabel string `json:"selfSourceLabel"`
	MaxFileSize     int64  `json:"maxFileSize"`
	MaxBackups      int    `json:"maxBackups"`
}

type Source struct {
	Type                  string `json:"type"`
	ID                    string `json:"id"`
	Label                 string `json:"label"`
	Description           string `json:"description,omitempty"`
	Hint                  string `json:"hint,omitempty"`
	Path                  string `json:"path,omitempty"`
	Container             string `json:"container,omitempty"`
	Host                  string `json:"host,omitempty"`
	Port                  int    `json:"port,omitempty"`
	User                  string `json:"user,omitempty"`
	IdentityFile          string `json:"identityFile,omitempty"`
	StrictHostKeyChecking bool   `json:"strictHostKeyChecking,omitempty"`
	// Disabled keeps a source in the file but hides it from agents.
	Disabled bool `json:"disabled,omitempty"`
	// Multiplex shares one ssh connection between calls. It is an internal
	// switch set by the plugin ssh capability, not a configured option, so
	// it is never read from (or written to) JSON.
	Multiplex bool `json:"-"`
}

// UnmarshalJSON applies per-source defaults (type=local, ssh port=22).
func (s *Source) UnmarshalJSON(data []byte) error {
	type plain Source
	p := plain{Type: SourceLocal}
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	if p.Type == SourceSSH && p.Port == 0 {
		p.Port = 22
	}
	*s = Source(p)
	return nil
}

// Remote reports whether the source is reached through ssh.
func (s Source) Remote() bool {
	return s.Type == SourceSSH || (s.Type == SourceDocker && s.Host != "")
}

// Validate checks a single source definition.
func (s Source) Validate() error {
	if s.ID == "" || s.Label == "" {
		return errors.New("id and label are required")
	}
	switch s.Type {
	case SourceLocal:
		if s.Path == "" {
			return errors.New("path is required")
		}
	case SourceSSH:
		if s.Path == "" {
			return errors.New("path is required")
		}
		if s.Host == "" || s.User == "" || s.IdentityFile == "" {
			return errors.New("host, user and identityFile are required for ssh sources")
		}
		if s.Port < 1 || s.Port > 65535 {
			return fmt.Errorf("port out of range: %d", s.Port)
		}
	case SourceDocker:
		if !containerName.MatchString(s.Container) {
			return fmt.Errorf("invalid container name %q (letters, digits, '_', '.', '-')", s.Container)
		}
		if s.Port < 0 || s.Port > 65535 {
			return fmt.Errorf("port out of range: %d", s.Port)
		}
	default:
		return fmt.Errorf("unknown source type %q", s.Type)
	}
	return nil
}

type Config struct {
	Sources   []Source  `json:"sources"`
	SkillsDir string    `json:"skillsDir"`
	Transport Transport `json:"transport"`
	Logging   Logging   `json:"logging"`
	// PluginDirs are searched for Lua plugins (default: <config dir>/plugins).
	PluginDirs []string    `json:"pluginDirs"`
	Plugins    []PluginRef `json:"plugins"`
}

// EnabledSources returns the sources agents may use (everything not disabled).
func (c Config) EnabledSources() []Source {
	out := make([]Source, 0, len(c.Sources))
	for _, s := range c.Sources {
		if !s.Disabled {
			out = append(out, s)
		}
	}
	return out
}

// Defaults returns the built-in configuration. Relative paths are resolved by
// Load/Finalize against the directory of the config file.
func Defaults() Config {
	return Config{
		Sources: []Source{},
		Transport: Transport{
			Mode: TransportStdio,
			Host: "127.0.0.1",
			Port: 3333,
			Path: "/mcp",
		},
		Logging: Logging{
			Enabled:         true,
			MaxBodyChars:    4000,
			SelfSourceID:    "mcplogview-self",
			SelfSourceLabel: "MCP Server Self Log",
			MaxFileSize:     10 * 1024 * 1024,
			MaxBackups:      5,
		},
	}
}

// Load reads and validates the config file at path.
func Load(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	cfg := Defaults()
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return Config{}, err
	}
	if err := cfg.Finalize(filepath.Dir(abs)); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Finalize validates the config, resolves relative paths against baseDir and
// prepends the self-log source when logging is enabled.
func (c *Config) Finalize(baseDir string) error {
	if err := c.validate(); err != nil {
		return err
	}

	if c.Logging.Path == "" {
		c.Logging.Path = filepath.Join(StateDir(), "mcplogview-server.log")
	}
	if c.SkillsDir == "" {
		c.SkillsDir = filepath.Join(baseDir, "log-skills")
	}

	c.Logging.Path = resolvePath(baseDir, c.Logging.Path)
	c.SkillsDir = resolvePath(baseDir, c.SkillsDir)
	c.Transport.HTTPSKey = resolvePath(baseDir, c.Transport.HTTPSKey)
	c.Transport.HTTPSCert = resolvePath(baseDir, c.Transport.HTTPSCert)
	c.finalizePlugins(baseDir)
	for i := range c.Sources {
		c.Sources[i].Path = resolvePathFor(c.Sources[i], baseDir)
		c.Sources[i].IdentityFile = resolvePath(baseDir, c.Sources[i].IdentityFile)
	}

	if c.Logging.Enabled {
		exists := false
		for _, s := range c.Sources {
			if s.ID == c.Logging.SelfSourceID {
				exists = true
				break
			}
		}
		if !exists {
			self := Source{
				Type:        SourceLocal,
				ID:          c.Logging.SelfSourceID,
				Label:       c.Logging.SelfSourceLabel,
				Description: "Internal mcplogview server log",
				Path:        c.Logging.Path,
			}
			c.Sources = append([]Source{self}, c.Sources...)
		}
	}

	seen := make(map[string]struct{}, len(c.Sources))
	for _, s := range c.Sources {
		if _, dup := seen[s.ID]; dup {
			return fmt.Errorf("duplicate source id: %s", s.ID)
		}
		seen[s.ID] = struct{}{}
	}
	return nil
}

func (c *Config) validate() error {
	switch c.Transport.Mode {
	case TransportStdio, TransportHTTP, TransportHTTPSDev:
	default:
		return fmt.Errorf("transport.mode must be one of stdio, http, https-dev (got %q)", c.Transport.Mode)
	}
	if c.Transport.Host == "" || c.Transport.Path == "" {
		return errors.New("transport.host and transport.path must not be empty")
	}
	if c.Transport.Port < 1 || c.Transport.Port > 65535 {
		return fmt.Errorf("transport.port out of range: %d", c.Transport.Port)
	}
	if c.Transport.AuthToken != "" && len(c.Transport.AuthToken) < 16 {
		return errors.New("transport.authToken must be at least 16 characters")
	}
	if c.Logging.MaxBodyChars < 128 || c.Logging.MaxBodyChars > 20000 {
		return fmt.Errorf("logging.maxBodyChars must be 128..20000 (got %d)", c.Logging.MaxBodyChars)
	}
	if c.Logging.MaxFileSize < 1024 || c.Logging.MaxFileSize > 1<<30 {
		return fmt.Errorf("logging.maxFileSize must be 1024..1073741824 (got %d)", c.Logging.MaxFileSize)
	}
	if c.Logging.MaxBackups < 1 || c.Logging.MaxBackups > 100 {
		return fmt.Errorf("logging.maxBackups must be 1..100 (got %d)", c.Logging.MaxBackups)
	}
	if c.Logging.SelfSourceID == "" || c.Logging.SelfSourceLabel == "" {
		return errors.New("logging.selfSourceId and logging.selfSourceLabel must not be empty")
	}

	for i, s := range c.Sources {
		if err := s.Validate(); err != nil {
			return fmt.Errorf("sources[%d] (%s): %w", i, s.ID, err)
		}
	}
	return c.validatePlugins()
}

// resolvePathFor leaves remote (ssh) paths untouched: they are not local files.
func resolvePathFor(s Source, baseDir string) string {
	if s.Type != SourceLocal {
		return s.Path
	}
	return resolvePath(baseDir, s.Path)
}

func resolvePath(baseDir, p string) string {
	if p == "" {
		return p
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(baseDir, p)
}
