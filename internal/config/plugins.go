package config

import (
	"fmt"
	"net"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

const (
	// CapSSH and CapFS are the capabilities a plugin can be granted.
	CapSSH = "ssh"
	CapFS  = "fs"

	maxPluginTimeoutSeconds = 600
)

var pluginName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// PluginRef configures one Lua plugin and the capabilities it is granted.
type PluginRef struct {
	Name           string         `json:"name"`
	Enabled        *bool          `json:"enabled,omitempty"`
	Allow          []string       `json:"allow,omitempty"`
	SSH            *PluginSSH     `json:"ssh,omitempty"`
	FS             *PluginFS      `json:"fs,omitempty"`
	TimeoutSeconds int            `json:"timeoutSeconds,omitempty"`
	Opts           map[string]any `json:"opts,omitempty"`
	// Git is the repository `mcplogview plugin sync` installs the plugin from.
	Git string `json:"git,omitempty"`
	// Ref is the branch, tag or commit to check out (default: the remote HEAD).
	Ref string `json:"ref,omitempty"`
}

// PluginSSH is the ssh capability: where a plugin may connect and as whom.
type PluginSSH struct {
	AllowedCIDRs          []string `json:"allowedCIDRs"`
	User                  string   `json:"user"`
	IdentityFile          string   `json:"identityFile"`
	Port                  int      `json:"port,omitempty"`
	StrictHostKeyChecking bool     `json:"strictHostKeyChecking,omitempty"`
}

// PluginFS is the fs capability: the local directories a plugin may read.
type PluginFS struct {
	Roots []string `json:"roots"`
}

// IsEnabled reports whether the plugin should be loaded (default true).
func (p PluginRef) IsEnabled() bool { return p.Enabled == nil || *p.Enabled }

// Allows reports whether the plugin was granted the capability.
func (p PluginRef) Allows(capability string) bool { return slices.Contains(p.Allow, capability) }

func (p PluginRef) validate() error {
	if !pluginName.MatchString(p.Name) {
		return fmt.Errorf("invalid name %q (lowercase letters, digits, '_', '-')", p.Name)
	}
	if p.TimeoutSeconds < 0 || p.TimeoutSeconds > maxPluginTimeoutSeconds {
		return fmt.Errorf("timeoutSeconds must be 0..%d (got %d)", maxPluginTimeoutSeconds, p.TimeoutSeconds)
	}
	if err := gitArg("git", p.Git); err != nil {
		return err
	}
	if err := gitArg("ref", p.Ref); err != nil {
		return err
	}
	if p.Ref != "" && p.Git == "" {
		return fmt.Errorf("ref requires git")
	}
	for _, a := range p.Allow {
		if a != CapSSH && a != CapFS {
			return fmt.Errorf("unknown allow value %q (ssh, fs)", a)
		}
	}
	if p.Allows(CapSSH) {
		s := p.SSH
		if s == nil {
			return fmt.Errorf("allow %q requires an ssh block", CapSSH)
		}
		if len(s.AllowedCIDRs) == 0 {
			return fmt.Errorf("ssh.allowedCIDRs must not be empty")
		}
		for _, c := range s.AllowedCIDRs {
			if _, _, err := net.ParseCIDR(c); err != nil {
				return fmt.Errorf("ssh.allowedCIDRs: invalid CIDR %q", c)
			}
		}
		if s.User == "" || s.IdentityFile == "" {
			return fmt.Errorf("ssh.user and ssh.identityFile are required")
		}
		if s.Port < 0 || s.Port > 65535 {
			return fmt.Errorf("ssh.port out of range: %d", s.Port)
		}
	}
	if p.Allows(CapFS) {
		if p.FS == nil || len(p.FS.Roots) == 0 {
			return fmt.Errorf("allow %q requires fs.roots", CapFS)
		}
		for _, r := range p.FS.Roots {
			if r == "" {
				return fmt.Errorf("fs.roots must not contain empty paths")
			}
		}
	}
	return nil
}

// gitArg rejects values that git could misread as options or that would not
// survive as one argument.
func gitArg(field, v string) error {
	if v == "" {
		return nil
	}
	if strings.HasPrefix(v, "-") {
		return fmt.Errorf("%s must not start with '-' (got %q)", field, v)
	}
	if strings.ContainsFunc(v, func(r rune) bool { return r == 0 || unicode.IsSpace(r) }) {
		return fmt.Errorf("%s must not contain whitespace or NUL (got %q)", field, v)
	}
	return nil
}

func (c *Config) validatePlugins() error {
	seen := make(map[string]struct{}, len(c.Plugins))
	for i, p := range c.Plugins {
		if err := p.validate(); err != nil {
			return fmt.Errorf("plugins[%d] (%s): %w", i, p.Name, err)
		}
		if _, dup := seen[p.Name]; dup {
			return fmt.Errorf("plugins[%d]: duplicate plugin name: %s", i, p.Name)
		}
		seen[p.Name] = struct{}{}
	}
	return nil
}

func (c *Config) finalizePlugins(baseDir string) {
	if len(c.PluginDirs) == 0 {
		c.PluginDirs = []string{"plugins"}
	}
	for i, d := range c.PluginDirs {
		c.PluginDirs[i] = resolvePath(baseDir, d)
	}
	for i := range c.Plugins {
		p := &c.Plugins[i]
		if p.SSH != nil {
			p.SSH.IdentityFile = resolvePath(baseDir, p.SSH.IdentityFile)
			if p.SSH.Port == 0 {
				p.SSH.Port = 22
			}
		}
		if p.FS != nil {
			for j, r := range p.FS.Roots {
				p.FS.Roots[j] = resolvePath(baseDir, r)
			}
		}
	}
}
