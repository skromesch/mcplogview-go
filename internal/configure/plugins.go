package configure

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// field is one key of a JSON object, kept in file order.
type field struct {
	key string
	val json.RawMessage
}

// decodeObject reads a JSON object keeping its key order.
func decodeObject(raw json.RawMessage) ([]field, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, fmt.Errorf("expected a JSON object")
	}
	var out []field
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		out = append(out, field{t.(string), v})
	}
	return out, nil
}

func encodeObject(fields []field) json.RawMessage {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, f := range fields {
		if i > 0 {
			buf.WriteByte(',')
		}
		k, _ := json.Marshal(f.key)
		buf.Write(k)
		buf.WriteByte(':')
		buf.Write(f.val)
	}
	buf.WriteByte('}')
	return buf.Bytes()
}

// pluginObjects returns the "plugins" array, each entry with its keys in order.
func (f *File) pluginObjects() ([][]field, error) {
	raw, ok := f.raw["plugins"]
	if !ok {
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("parse plugins in %s: %w", f.Path, err)
	}
	out := make([][]field, 0, len(items))
	for _, it := range items {
		obj, err := decodeObject(it)
		if err != nil {
			return nil, fmt.Errorf("parse plugins in %s: %w", f.Path, err)
		}
		out = append(out, obj)
	}
	return out, nil
}

// PluginState is a configured plugin's name and on/off switch.
type PluginState struct {
	Name    string
	Enabled bool
}

// Plugins lists the plugins configured in the file, in file order.
func (f *File) Plugins() ([]PluginState, error) {
	objs, err := f.pluginObjects()
	if err != nil {
		return nil, err
	}
	out := make([]PluginState, 0, len(objs))
	for _, obj := range objs {
		p := PluginState{Enabled: true}
		for _, fl := range obj {
			switch fl.key {
			case "name":
				_ = json.Unmarshal(fl.val, &p.Name)
			case "enabled":
				var b bool
				if json.Unmarshal(fl.val, &b) == nil {
					p.Enabled = b
				}
			}
		}
		out = append(out, p)
	}
	return out, nil
}

// HasPlugin reports whether a plugin with that name is configured.
func (f *File) HasPlugin(name string) bool {
	ps, _ := f.Plugins()
	for _, p := range ps {
		if p.Name == name {
			return true
		}
	}
	return false
}

// SetPluginEnabled switches a plugin on or off. Every other key of the plugin
// entry stays as it is, in its order; an existing "enabled" key is updated in
// place, and a missing one is only added when switching off.
func (f *File) SetPluginEnabled(name string, enabled bool) error {
	objs, err := f.pluginObjects()
	if err != nil {
		return err
	}
	found := false
	for i, obj := range objs {
		var n string
		for _, fl := range obj {
			if fl.key == "name" {
				_ = json.Unmarshal(fl.val, &n)
			}
		}
		if n != name {
			continue
		}
		found = true
		val := json.RawMessage(fmt.Sprint(enabled))
		set := false
		for j := range obj {
			if obj[j].key == "enabled" {
				obj[j].val, set = val, true
			}
		}
		if !set && !enabled {
			objs[i] = append(obj, field{"enabled", val})
		}
	}
	if !found {
		return fmt.Errorf("no plugin named %q", name)
	}
	items := make([]json.RawMessage, len(objs))
	for i, obj := range objs {
		items[i] = encodeObject(obj)
	}
	b, err := json.Marshal(items)
	if err != nil {
		return err
	}
	f.raw["plugins"] = b
	return nil
}
