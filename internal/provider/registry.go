package provider

import (
	"fmt"

	"github.com/Shashank-Panda/relay/internal/domain"
)

// Registry maps a provider ID to its adapter.
//
// Built once at startup and read-only thereafter, so it needs no lock. That is
// a deliberate constraint rather than an oversight: adapters hold connection
// pools, and a registry that could be mutated at runtime would invite swapping
// one out from under in-flight requests.
type Registry struct {
	adapters map[domain.ProviderID]Adapter
}

func NewRegistry(adapters ...Adapter) *Registry {
	r := &Registry{adapters: make(map[domain.ProviderID]Adapter, len(adapters))}
	for _, a := range adapters {
		if a != nil {
			r.adapters[a.ID()] = a
		}
	}
	return r
}

// ErrNoAdapter means the catalog names a provider nothing implements. A
// configuration error, caught at the first request to that endpoint.
type ErrNoAdapter struct{ Provider domain.ProviderID }

func (e *ErrNoAdapter) Error() string {
	return fmt.Sprintf("no adapter registered for provider %q", e.Provider)
}

func (r *Registry) For(p domain.ProviderID) (Adapter, error) {
	if r == nil {
		return nil, &ErrNoAdapter{Provider: p}
	}
	a, ok := r.adapters[p]
	if !ok {
		return nil, &ErrNoAdapter{Provider: p}
	}
	return a, nil
}

// Providers lists the registered provider IDs in sorted order, for startup
// logging and for validating a catalog against what this build can actually
// reach.
func (r *Registry) Providers() []domain.ProviderID {
	if r == nil {
		return nil
	}
	byName := make(map[string]domain.ProviderID, len(r.adapters))
	for p := range r.adapters {
		byName[string(p)] = p
	}
	names := domain.SortedKeys(byName)
	out := make([]domain.ProviderID, 0, len(names))
	for _, n := range names {
		out = append(out, byName[n])
	}
	return out
}

// Unreachable reports catalog endpoints whose provider has no adapter.
//
// Reported at startup rather than discovered at request time. An endpoint that
// routing can select but execution cannot reach is a latent outage sitting in a
// config file, and the operator who added it is the person best placed to fix
// it — which means telling them while they are still watching the deploy.
func (r *Registry) Unreachable(cat *domain.Catalog) []string {
	if cat == nil {
		return nil
	}
	var out []string
	for _, id := range domain.SortedKeys(cat.Endpoints) {
		if _, err := r.For(cat.Endpoints[id].Provider); err != nil {
			out = append(out, id)
		}
	}
	return out
}
