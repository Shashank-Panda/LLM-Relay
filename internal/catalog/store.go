package catalog

import (
	"sync/atomic"

	"github.com/Shashank-Panda/relay/internal/domain"
)

// Store holds the live catalog snapshot.
//
// The catalog is read on every single request and written approximately never —
// a price change, a new model, a retirement. That ratio dictates the mechanism.
//
// An RWMutex would be correct and would still be wrong: it puts atomic
// read-lock traffic on the hot path of every request to protect a value that
// changes on the order of days, and under high concurrency the shared cache line
// becomes contended by readers alone. An atomic pointer load compiles to an
// ordinary load on every architecture Relay targets.
//
// The second property matters more than the speed. Swaps replace the entire
// snapshot at once, so there is no window in which endpoints have been updated
// and routes have not. A request that begins under catalog version N completes
// under version N, and because Route is pure and the Decision records that
// version, the decision can be replayed exactly afterwards.
type Store struct {
	current atomic.Pointer[domain.Catalog]
}

// NewStore returns a store holding cat, which may be nil.
func NewStore(cat *domain.Catalog) *Store {
	s := &Store{}
	if cat != nil {
		s.current.Store(cat)
	}
	return s
}

// Current returns the live snapshot. Callers must treat it as immutable: it is
// shared with every other in-flight request.
func (s *Store) Current() *domain.Catalog { return s.current.Load() }

// Swap installs a new snapshot and returns the previous one.
func (s *Store) Swap(next *domain.Catalog) *domain.Catalog {
	if next == nil {
		return s.current.Load()
	}
	return s.current.Swap(next)
}

// ReloadFile loads, validates, and only then installs a catalog.
//
// The ordering is the whole point. A malformed or stale catalog leaves the
// previous snapshot serving traffic and returns an error for the operator to
// act on. An operator's typo at 3am must degrade to "running yesterday's
// prices", never to "not running" — see ADR-0010.
func (s *Store) ReloadFile(path string, opts Options) error {
	next, err := LoadFile(path, opts)
	if err != nil {
		return err
	}
	s.Swap(next)
	return nil
}
