package provider

import "fmt"

// Registry holds registered provider adapters, keyed by Kind.
type Registry struct {
	adapters map[Kind]Adapter
}

// NewRegistry creates an empty adapter registry.
func NewRegistry() *Registry {
	return &Registry{adapters: make(map[Kind]Adapter)}
}

// Register adds an adapter to the registry.
func (r *Registry) Register(a Adapter) {
	r.adapters[a.Kind()] = a
}

// Get returns the adapter for the given kind, or an error if not registered.
func (r *Registry) Get(kind Kind) (Adapter, error) {
	a, ok := r.adapters[kind]
	if !ok {
		return nil, fmt.Errorf("no adapter registered for provider %q", kind)
	}
	return a, nil
}
