package logs

import "github.com/skromesch/mcplogview-go/internal/config"

// Registry gives ordered, id-based access to the configured sources.
type Registry struct {
	order []config.Source
	byID  map[string]config.Source
}

func NewRegistry(sources []config.Source) *Registry {
	r := &Registry{order: append([]config.Source(nil), sources...), byID: make(map[string]config.Source, len(sources))}
	for _, s := range sources {
		r.byID[s.ID] = s
	}
	return r
}

func (r *Registry) List() []config.Source { return append([]config.Source(nil), r.order...) }

func (r *Registry) Get(id string) (config.Source, bool) {
	s, ok := r.byID[id]
	return s, ok
}
