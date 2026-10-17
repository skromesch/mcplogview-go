package logs

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/skromesch/mcplogview-go/internal/config"
)

// DirEntry is one item of a directory listing.
type DirEntry struct {
	Name  string
	IsDir bool
}

// Container is a docker container as listed by `docker ps -a`.
type Container struct {
	Name, Image, Status string
}

// ListDir lists dir on the machine described by loc (local when loc has no
// Host). Directories come first, then files, each alphabetically.
func ListDir(loc config.Source, dir string) ([]DirEntry, error) {
	var entries []DirEntry
	if loc.Host == "" {
		items, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			isDir := it.IsDir()
			if it.Type()&os.ModeSymlink != 0 {
				if st, err := os.Stat(filepath.Join(dir, it.Name())); err == nil {
					isDir = st.IsDir()
				}
			}
			entries = append(entries, DirEntry{Name: it.Name(), IsDir: isDir})
		}
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), sshTimeout)
		defer cancel()
		out, err := runScript(ctx, loc, fmt.Sprintf(`LC_ALL=C ls -1Ap -- %s`, shellQuote(dir)))
		if err != nil {
			return nil, err
		}
		for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if l == "" {
				continue
			}
			if strings.HasSuffix(l, "/") {
				entries = append(entries, DirEntry{Name: strings.TrimSuffix(l, "/"), IsDir: true})
			} else {
				entries = append(entries, DirEntry{Name: l})
			}
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return entries[i].Name < entries[j].Name
	})
	return entries, nil
}

// ParentDir returns the parent of a slash-separated path ("/" stays "/").
func ParentDir(dir string) string { return path.Dir(path.Clean(dir)) }

// ListContainers lists all containers (running or not) on loc's docker host.
func ListContainers(loc config.Source) ([]Container, error) {
	ctx, cancel := context.WithTimeout(context.Background(), sshTimeout)
	defer cancel()

	script := `command -v docker >/dev/null 2>&1 || { echo "` + errPrefix + `nodocker"; exit 13; }; ` +
		`docker ps -a --format '{{.Names}}\t{{.Image}}\t{{.Status}}'`
	out, err := runScript(ctx, loc, script)
	if strings.HasPrefix(string(out), errPrefix) {
		return nil, parseDockerFailure(loc, out)
	}
	if err != nil {
		return nil, err
	}

	var res []Container
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(l, "\t", 3)
		if len(parts) < 3 || parts[0] == "" {
			continue
		}
		res = append(res, Container{Name: parts[0], Image: parts[1], Status: parts[2]})
	}
	sort.SliceStable(res, func(i, j int) bool { return res[i].Name < res[j].Name })
	return res, nil
}
