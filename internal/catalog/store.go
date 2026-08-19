package catalog

import (
	"sync/atomic"
	"time"

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

	// loadedAt is when the live snapshot was installed, in Unix nanoseconds.
	//
	// Tracked because ADR-0010 calls a stale catalog the sharpest edge of
	// failing open. Serving from the last good snapshot is right when the
	// control plane is briefly unreachable and wrong when it has been
	// unreachable for a day: a retired model keeps being selected, and a price
	// change goes unapplied while every savings figure computed against it
	// quietly drifts. Old data is more dangerous than no data, because the
	// system stays confident.
	loadedAt atomic.Int64

	// maxAge is when a snapshot stops being trustworthy enough to route on.
	// Zero disables the check.
	maxAge atomic.Int64
}

// NewStore returns a store holding cat, which may be nil.
func NewStore(cat *domain.Catalog) *Store {
	s := &Store{}
	if cat != nil {
		s.current.Store(cat)
		s.loadedAt.Store(time.Now().UnixNano())
	}
	return s
}

// WithMaxAge sets when a snapshot becomes too old to route on.
//
// Not too old to *serve* on: past this age the gateway degrades to baseline
// passthrough, which still answers every request using the model the caller
// named. What it stops doing is making cost and quality decisions from prices
// and lifecycle data nobody has confirmed recently.
func (s *Store) WithMaxAge(d time.Duration) *Store {
	s.maxAge.Store(int64(d))
	return s
}

// Age is how long the live snapshot has been installed.
func (s *Store) Age() time.Duration {
	at := s.loadedAt.Load()
	if at == 0 {
		return 0
	}
	return time.Since(time.Unix(0, at))
}

// Stale reports whether the snapshot has passed its maximum age.
func (s *Store) Stale() bool {
	max := s.maxAge.Load()
	if max <= 0 {
		return false
	}
	return s.Age() > time.Duration(max)
}

// Current returns the live snapshot. Callers must treat it as immutable: it is
// shared with every other in-flight request.
func (s *Store) Current() *domain.Catalog { return s.current.Load() }

// Swap installs a new snapshot and returns the previous one.
func (s *Store) Swap(next *domain.Catalog) *domain.Catalog {
	if next == nil {
		return s.current.Load()
	}
	prev := s.current.Swap(next)
	s.loadedAt.Store(time.Now().UnixNano())
	return prev
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
