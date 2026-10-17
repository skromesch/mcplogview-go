package skills

import (
	"strconv"
	"strings"
)

// Meta is the parsed front matter of a skill file.
type Meta struct {
	Name     string
	SourceID string
	Hint     string
	Version  int
	Tags     []string
}

// Parsed is the result of ParseMarkdown.
type Parsed struct {
	Meta     Meta
	Body     string
	Warnings []string
}

func defaultMeta() Meta { return Meta{Version: 1, Tags: []string{}} }

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func parseTags(value string) []string {
	v := strings.TrimSpace(value)
	tags := []string{}
	if strings.HasPrefix(v, "[") && strings.HasSuffix(v, "]") {
		for _, part := range strings.Split(v[1:len(v)-1], ",") {
			t := strings.Trim(strings.TrimSpace(part), `"'`)
			if t != "" {
				tags = append(tags, t)
			}
		}
		return tags
	}
	if v != "" {
		tags = append(tags, v)
	}
	return tags
}

// ParseMarkdown splits a skill file into front matter and body. Invalid front
// matter degrades to defaults plus a warning, it never fails.
func ParseMarkdown(raw string) Parsed {
	if !strings.HasPrefix(raw, "---\n") {
		return Parsed{Meta: defaultMeta(), Body: raw, Warnings: []string{"missing front matter header; using defaults"}}
	}
	end := strings.Index(raw[4:], "\n---\n")
	if end < 0 {
		return Parsed{Meta: defaultMeta(), Body: raw, Warnings: []string{"invalid front matter delimiter; using defaults"}}
	}
	end += 4

	header, body := raw[4:end], raw[end+5:]
	warnings := []string{}
	values := map[string]string{}
	var tags []string

	for _, line := range strings.Split(header, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		sep := strings.Index(t, ":")
		if sep < 0 {
			warnings = append(warnings, "ignored malformed front matter line: "+t)
			continue
		}
		key, value := strings.TrimSpace(t[:sep]), strings.TrimSpace(t[sep+1:])
		if key == "tags" {
			tags = parseTags(value)
		} else {
			values[key] = value
		}
	}

	meta := defaultMeta()
	if tags != nil {
		meta.Tags = tags
	}
	invalid := false

	// Text fields must be non-empty and not purely numeric (matches the
	// original zod schema, where digit-only scalars became numbers).
	for key, dst := range map[string]*string{"name": &meta.Name, "sourceId": &meta.SourceID, "hint": &meta.Hint} {
		v, ok := values[key]
		if !ok {
			continue
		}
		if v == "" || isDigits(v) {
			invalid = true
			continue
		}
		*dst = v
	}
	if v, ok := values["version"]; ok {
		n, err := strconv.Atoi(v)
		if !isDigits(v) || err != nil || n < 1 {
			invalid = true
		} else {
			meta.Version = n
		}
	}

	if invalid {
		return Parsed{Meta: defaultMeta(), Body: body, Warnings: append(warnings, "invalid front matter values; using defaults")}
	}
	return Parsed{Meta: meta, Body: body, Warnings: warnings}
}
