// Package configure edits mcplogview config files without disturbing the
// settings it does not manage, and provides helpers for the interactive wizard.
package configure

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/skromesch/mcplogview-go/internal/config"
)

// File is a config file on disk. Unknown or unmanaged keys are kept verbatim.
type File struct {
	Path    string
	raw     map[string]json.RawMessage
	Sources []config.Source
}

// keyOrder is the preferred top-level key order when writing.
var keyOrder = []string{"transport", "logging", "skillsDir", "sources"}

// Open reads path, or returns an empty File if it does not exist yet.
func Open(path string) (*File, error) {
	f := &File{Path: path, raw: map[string]json.RawMessage{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, &f.raw); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	if rawSources, ok := f.raw["sources"]; ok {
		if err := json.Unmarshal(rawSources, &f.Sources); err != nil {
			return nil, fmt.Errorf("parse sources in %s: %w", path, err)
		}
	}
	return f, nil
}

// Find returns the index of the source with the given id, or -1.
func (f *File) Find(id string) int {
	for i, s := range f.Sources {
		if s.ID == id {
			return i
		}
	}
	return -1
}

// Add validates and appends a source. Ids must be unique.
func (f *File) Add(s Source) error {
	src := Normalize(s)
	if err := src.Validate(); err != nil {
		return err
	}
	if f.Find(src.ID) >= 0 {
		return fmt.Errorf("source %q already exists", src.ID)
	}
	f.Sources = append(f.Sources, src)
	return nil
}

// Remove deletes a source by id.
func (f *File) Remove(id string) error {
	i := f.Find(id)
	if i < 0 {
		return fmt.Errorf("no source with id %q", id)
	}
	f.Sources = append(f.Sources[:i], f.Sources[i+1:]...)
	return nil
}

// SetDisabled switches a source off (true) or on (false).
func (f *File) SetDisabled(id string, disabled bool) error {
	i := f.Find(id)
	if i < 0 {
		return fmt.Errorf("no source with id %q", id)
	}
	f.Sources[i].Disabled = disabled
	return nil
}

// Source is an alias so callers do not need to import config just for this.
type Source = config.Source

// Normalize drops fields that do not apply to the source type, so files stay
// minimal and `ssh` sources get their default port.
func Normalize(s config.Source) config.Source {
	switch s.Type {
	case config.SourceLocal, "":
		s.Type = config.SourceLocal
		s.Container = ""
		s.Host, s.User, s.IdentityFile, s.Port, s.StrictHostKeyChecking = "", "", "", 0, false
	case config.SourceSSH:
		s.Container = ""
		if s.Port == 0 {
			s.Port = 22
		}
	case config.SourceDocker:
		s.Path = ""
		if s.Host == "" {
			s.User, s.IdentityFile, s.Port, s.StrictHostKeyChecking = "", "", 0, false
		}
	}
	return s
}

// fileMode keeps an existing file's permissions (never widening them) and
// restricts files holding an auth token to the owner.
func (f *File) fileMode() os.FileMode {
	mode := os.FileMode(0o644)
	if st, err := os.Stat(f.Path); err == nil {
		mode = st.Mode().Perm()
	}
	if t, ok := f.raw["transport"]; ok && bytes.Contains(t, []byte(`"authToken"`)) {
		mode = 0o600
	}
	return mode
}

// Transport returns the transport settings as written in the file, on top of
// the defaults.
func (f *File) Transport() (config.Transport, error) {
	t := config.Defaults().Transport
	if raw, ok := f.raw["transport"]; ok {
		if err := json.Unmarshal(raw, &t); err != nil {
			return t, fmt.Errorf("parse transport in %s: %w", f.Path, err)
		}
	}
	return t, nil
}

// UpdateTransport sets and removes keys inside the "transport" object while
// keeping every other key in it. A nil value removes the key.
func (f *File) UpdateTransport(changes map[string]any) error {
	obj := map[string]json.RawMessage{}
	if raw, ok := f.raw["transport"]; ok {
		if err := json.Unmarshal(raw, &obj); err != nil {
			return fmt.Errorf("parse transport in %s: %w", f.Path, err)
		}
	}
	for k, v := range changes {
		if v == nil {
			delete(obj, k)
			continue
		}
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		obj[k] = b
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	f.raw["transport"] = b
	return nil
}

// NewToken returns a random URL-safe auth token (192 bits).
func NewToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Save writes the file atomically (temp file + rename), creating directories.
func (f *File) Save() error {
	if f.Sources == nil {
		f.Sources = []config.Source{}
	}
	srcJSON, err := json.Marshal(f.Sources)
	if err != nil {
		return err
	}
	f.raw["sources"] = srcJSON

	var buf bytes.Buffer
	buf.WriteString("{\n")
	keys := orderedKeys(f.raw)
	for i, k := range keys {
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, f.raw[k], "  ", "  "); err != nil {
			return err
		}
		name, _ := json.Marshal(k)
		fmt.Fprintf(&buf, "  %s: %s", name, pretty.String())
		if i < len(keys)-1 {
			buf.WriteString(",")
		}
		buf.WriteString("\n")
	}
	buf.WriteString("}\n")

	if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(f.Path), ".mcplogview-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), f.fileMode()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), f.Path)
}

func orderedKeys(m map[string]json.RawMessage) []string {
	seen := map[string]bool{}
	var keys []string
	for _, k := range keyOrder {
		if _, ok := m[k]; ok {
			keys = append(keys, k)
			seen[k] = true
		}
	}
	var rest []string
	for k := range m {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	return append(keys, rest...)
}
