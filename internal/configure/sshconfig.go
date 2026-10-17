package configure

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SSHHost is what `ssh -G <alias>` resolves for a host.
type SSHHost struct {
	Alias        string
	HostName     string
	User         string
	Port         int
	IdentityFile string // first identity file that exists, "~"-collapsed
}

// sshGetConfig runs `ssh -G`. Replaced in tests.
var sshGetConfig = func(alias string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "ssh", "-G", alias).Output()
}

// ListSSHAliases returns the concrete (non-wildcard) Host aliases from
// ~/.ssh/config, following simple Include lines.
func ListSSHAliases() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	collectAliases(filepath.Join(home, ".ssh", "config"), filepath.Join(home, ".ssh"), seen, 0)
	out := make([]string, 0, len(seen))
	for a := range seen {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

func collectAliases(path, baseDir string, seen map[string]bool, depth int) {
	if depth > 5 {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(strings.TrimSpace(sc.Text()))
		if len(fields) < 2 {
			continue
		}
		switch strings.ToLower(fields[0]) {
		case "host":
			for _, h := range fields[1:] {
				if !strings.ContainsAny(h, "*?!") {
					seen[h] = true
				}
			}
		case "include":
			for _, pat := range fields[1:] {
				if !filepath.IsAbs(pat) {
					pat = filepath.Join(baseDir, pat)
				}
				matches, _ := filepath.Glob(pat)
				for _, m := range matches {
					collectAliases(m, baseDir, seen, depth+1)
				}
			}
		}
	}
}

// ResolveSSHHost asks ssh itself for the effective settings of alias.
func ResolveSSHHost(alias string) (SSHHost, error) {
	out, err := sshGetConfig(alias)
	if err != nil {
		return SSHHost{}, err
	}
	return parseSSHG(alias, out), nil
}

func parseSSHG(alias string, out []byte) SSHHost {
	h := SSHHost{Alias: alias, Port: 22}
	home, _ := os.UserHomeDir()
	for _, line := range strings.Split(string(bytes.TrimSpace(out)), "\n") {
		key, val, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch strings.ToLower(key) {
		case "hostname":
			h.HostName = val
		case "user":
			h.User = val
		case "port":
			if p, err := strconv.Atoi(val); err == nil {
				h.Port = p
			}
		case "identityfile":
			if h.IdentityFile != "" {
				continue
			}
			full := val
			if strings.HasPrefix(full, "~/") && home != "" {
				full = filepath.Join(home, full[2:])
			}
			if st, err := os.Stat(full); err == nil && !st.IsDir() {
				h.IdentityFile = val // keep "~" form: portable across machines
			}
		}
	}
	return h
}
