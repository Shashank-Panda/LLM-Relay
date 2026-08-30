package respcache

import (
	"container/list"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/provider"
)

// Entry is one stored answer.
//
// Stored in normalized form rather than as provider bytes or as an encoded
// OpenAI response. That is what lets the same entry serve a streaming and a
// non-streaming request — the stored answer is replayed into whichever shape the
// caller asked for — and it keeps a vendor's wire format out of a structure that
// outlives the request that produced it.
type Entry struct {
	// Scope and Endpoint are re-checked on read.
	//
	// Both are already inside the key, so this is redundant by design. A
	// SHA-256 collision is not a realistic threat; a bug in key construction is,
	// and the consequence of that bug is a cross-tenant prompt disclosure. The
	// cheap check that turns a breach into a miss is worth keeping.
	Scope    string
	Endpoint string

	ProviderID   string
	Parts        []domain.ContentPart
	FinishReason provider.FinishReason

	// Usage is what the original call reported. Kept so a hit can be priced:
	// the counterfactual for a cache hit is what those tokens would have cost,
	// and without the counts there is no saving to report.
	Usage provider.Usage

	StoredAt  time.Time
	ExpiresAt time.Time

	// bytes is the approximate footprint, for the size budget.
	bytes int
}

// Response renders the entry as a completed answer.
func (e *Entry) Response() *provider.Response {
	return &provider.Response{
		ID:           e.ProviderID,
		Parts:        e.Parts,
		FinishReason: e.FinishReason,
		Usage:        e.Usage,
	}
}

// Options configures a Store.
type Options struct {
	// MaxEntries bounds the entry count. Zero means DefaultMaxEntries.
	MaxEntries int

	// MaxBytes bounds the approximate total footprint. Zero means
	// DefaultMaxBytes.
	//
	// Two limits rather than one because they fail differently: a cache of
	// 10,000 one-line answers is small and a cache of 200 long documents is not,
	// and only bounding the count would let the second one exhaust the heap.
	MaxBytes int

	// Now is injected for tests. Zero means time.Now.
	Now func() time.Time
}

const (
	DefaultMaxEntries = 10_000
	DefaultMaxBytes   = 128 << 20 // 128 MiB
)

// Store is an in-process LRU with per-entry expiry.
//
// In-process, and that is a stated limitation rather than an oversight.
// Architecture §7 puts the response cache in Redis so it is shared across
// replicas; behind N instances this one gets roughly 1/N of the achievable hit
// rate. The same sequencing as the savings ledger applies — build the local path
// first, behind an interface, and add the shared implementation when the
// deployment has more than one instance to share between. Nothing about the
// call sites changes when it does.
type Store struct {
	mu    sync.Mutex
	items map[string]*list.Element
	order *list.List // front = most recently used
	bytes int

	maxEntries int
	maxBytes   int
	now        func() time.Time

	hits     atomic.Uint64
	misses   atomic.Uint64
	stores   atomic.Uint64
	evicted  atomic.Uint64
	expired  atomic.Uint64
	mismatch atomic.Uint64
}

type item struct {
	key   string
	entry *Entry
}

func New(opts Options) *Store {
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = DefaultMaxEntries
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Store{
		items:      map[string]*list.Element{},
		order:      list.New(),
		maxEntries: opts.MaxEntries,
		maxBytes:   opts.MaxBytes,
		now:        opts.Now,
	}
}

// Get returns a live entry for this key, scope, and endpoint.
//
// The scope and endpoint arguments are not conveniences — they are the second
// half of the isolation guarantee. A caller that has only a key cannot read
// anything out of this store.
func (s *Store) Get(key, scope, endpointID string) (*Entry, bool) {
	if s == nil || key == "" {
		return nil, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	el, ok := s.items[key]
	if !ok {
		s.misses.Add(1)
		return nil, false
	}
	it := el.Value.(*item)

	if s.now().After(it.entry.ExpiresAt) {
		// Evicted on read rather than by a sweeper goroutine. A background
		// sweeper is another thing that can outlive the process's usefulness,
		// and the LRU already reclaims space under pressure; expiry only has to
		// be correct at the moment somebody asks.
		s.removeLocked(el)
		s.expired.Add(1)
		s.misses.Add(1)
		return nil, false
	}

	if it.entry.Scope != scope || it.entry.Endpoint != endpointID {
		// Should be unreachable: both are inside the key. If it ever fires, the
		// key derivation has a bug, and this counter is how that gets noticed
		// before a customer notices it instead.
		s.mismatch.Add(1)
		s.misses.Add(1)
		return nil, false
	}

	s.order.MoveToFront(el)
	s.hits.Add(1)
	return it.entry, true
}

// Put stores an answer, evicting as needed.
func (s *Store) Put(key string, e *Entry, ttl time.Duration) {
	if s == nil || key == "" || e == nil {
		return
	}
	if ttl <= 0 {
		ttl = domain.DefaultCacheTTL
	}

	now := s.now()
	e.StoredAt = now
	e.ExpiresAt = now.Add(ttl)
	e.bytes = e.size()

	// An entry larger than the whole budget would evict everything else and then
	// not fit. Declining it keeps one pathological request from emptying the
	// cache for every other tenant.
	if e.bytes > s.maxBytes {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if el, ok := s.items[key]; ok {
		s.removeLocked(el)
	}

	el := s.order.PushFront(&item{key: key, entry: e})
	s.items[key] = el
	s.bytes += e.bytes
	s.stores.Add(1)

	for (len(s.items) > s.maxEntries || s.bytes > s.maxBytes) && s.order.Len() > 0 {
		back := s.order.Back()
		if back == el {
			// The entry just inserted is the only one left. Stop rather than
			// evict it: a store that immediately discards what it was asked to
			// keep is worse than a store slightly over budget.
			break
		}
		s.removeLocked(back)
		s.evicted.Add(1)
	}
}

func (s *Store) removeLocked(el *list.Element) {
	it := el.Value.(*item)
	s.order.Remove(el)
	delete(s.items, it.key)
	s.bytes -= it.entry.bytes
}

// Stats is the store's counters, for the admin report and metrics.
type Stats struct {
	Entries  int    `json:"entries"`
	Bytes    int    `json:"bytes"`
	Hits     uint64 `json:"hits"`
	Misses   uint64 `json:"misses"`
	Stores   uint64 `json:"stores"`
	Evicted  uint64 `json:"evicted"`
	Expired  uint64 `json:"expired"`
	Mismatch uint64 `json:"scope_mismatch"`
}

// HitRate is hits over lookups. Reported rather than left to the reader because
// the two counters are only meaningful as a ratio.
func (st Stats) HitRate() float64 {
	total := st.Hits + st.Misses
	if total == 0 {
		return 0
	}
	return float64(st.Hits) / float64(total)
}

func (s *Store) Stats() Stats {
	if s == nil {
		return Stats{}
	}
	s.mu.Lock()
	entries, bytes := len(s.items), s.bytes
	s.mu.Unlock()

	return Stats{
		Entries:  entries,
		Bytes:    bytes,
		Hits:     s.hits.Load(),
		Misses:   s.misses.Load(),
		Stores:   s.stores.Load(),
		Evicted:  s.evicted.Load(),
		Expired:  s.expired.Load(),
		Mismatch: s.mismatch.Load(),
	}
}

// size approximates an entry's footprint.
//
// Approximate is enough: it bounds memory, it does not account for it. Counting
// the string bytes plus a fixed per-part overhead tracks the real figure closely
// enough to keep the heap bounded, and the alternative is reflection on the hot
// path to compute a number that only feeds an inequality.
func (e *Entry) size() int {
	n := len(e.Scope) + len(e.Endpoint) + len(e.ProviderID) + 128
	for _, p := range e.Parts {
		n += len(p.Text) + len(p.Arguments) + len(p.ToolName) + len(p.ToolCallID) +
			len(p.URL) + len(p.Data) + len(p.MediaType) + 64
	}
	return n
}

// Reason names why a request was not cacheable, for dry-run and for the
// operator asking why a cache is empty.
type Reason string

const (
	ReasonCacheable       Reason = ""
	ReasonRouteDisabled   Reason = "route does not enable the response cache"
	ReasonTemperature     Reason = "temperature is above zero and the route does not allow caching non-deterministic requests"
	ReasonNoCacheHeader   Reason = "the request asked to bypass the cache"
	ReasonPinnedStrict    Reason = "X-Relay-Pin: strict requires a live provider call"
	ReasonNoTenantOrRoute Reason = "no tenant or endpoint to scope the entry to"

	// ReasonAnonymousBYOK declines to cache when a request supplied its own
	// provider credentials but no isolation principal could be derived from
	// them.
	//
	// Declining rather than guessing. The alternative is to fall back to the
	// tenant, and under caller-supplied credentials the tenant is not the
	// security boundary: two strangers can present the same tenant id — the
	// anonymous default — while holding different keys, and a shared entry
	// between them is one person's answer, generated on their key, served to
	// somebody else along with confirmation of what they asked.
	ReasonAnonymousBYOK Reason = "request-supplied credentials with no derivable cache principal"
)

// Cacheable decides whether a request may read from or write to the cache.
//
// One function for both directions on purpose. A rule applied on write but not
// on read serves entries the current rules forbid; applied on read but not write
// it fills the cache with entries nothing will ever read. The asymmetry is a
// class of bug that only shows up as a mysterious hit rate.
func Cacheable(rt *domain.Route, req *domain.NormalizedRequest, tenant string) (Reason, bool) {
	if rt == nil || !rt.Cache.Enabled {
		return ReasonRouteDisabled, false
	}
	if tenant == "" {
		// Without a tenant there is no scope to key on, and an unscoped entry is
		// exactly the cross-tenant hit this package must be unable to produce.
		return ReasonNoTenantOrRoute, false
	}
	if req.NoCache {
		return ReasonNoCacheHeader, false
	}
	if t := req.Params.Temperature; t != nil && *t > 0 && !rt.Cache.AllowTemperature {
		return ReasonTemperature, false
	}
	return ReasonCacheable, true
}
