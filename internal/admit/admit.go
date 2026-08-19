// Package admit bounds how much work is in flight at once.
//
// The purpose is not throughput. A gateway with no admission control does not
// fail by getting slower — it fails by accepting every request, holding all of
// them until each one's deadline expires, and then returning errors for work it
// already paid a provider to perform. Shedding early converts that into a fast
// 503 the caller can retry, and a 503 in ten microseconds is a better answer
// than a 504 in sixty seconds.
//
// Three separate gates, because they protect different resources:
//
//   - Global, bounding total concurrent provider calls.
//   - Streaming, counted apart from everything else. Streams are long-lived and
//     nearly free in CPU while being expensive in file descriptors, memory, and
//     goroutines — so a limit sized for non-streaming throughput is far too
//     loose for them, and one sized for streams throttles ordinary traffic for
//     no reason.
//   - Per endpoint, so one slow provider cannot consume every global slot and
//     starve the endpoints that are still healthy. This is the gate that
//     matters during a partial outage, which is the common case.
package admit

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Config bounds each gate. A zero limit disables that gate entirely.
type Config struct {
	// MaxInFlight is the total concurrent provider calls.
	MaxInFlight int

	// MaxStreams is the concurrent streaming responses, counted within
	// MaxInFlight rather than in addition to it.
	MaxStreams int

	// MaxPerEndpoint is the concurrent calls to any one endpoint.
	MaxPerEndpoint int

	// QueueWait is how long a request may wait for a slot before being shed.
	//
	// Bounded, and deliberately short. An unbounded wait is not a queue, it is a
	// place requests go to expire — and every second spent waiting is a second
	// removed from the provider call's own budget.
	QueueWait time.Duration

	// MaxQueued caps how many requests may be waiting at once. Without it the
	// queue is bounded only by the number of goroutines the process can hold,
	// which is a limit discovered by running out of memory.
	MaxQueued int

	// RetryAfter is advertised to a shed caller.
	RetryAfter time.Duration
}

func DefaultConfig() Config {
	return Config{
		// Sized against the SLO's 1,000 rps at a one-second mean provider
		// latency, with headroom. It is a backstop, not a throttle: reaching it
		// means something upstream is already wrong.
		MaxInFlight:    2048,
		MaxStreams:     1024,
		MaxPerEndpoint: 512,
		// A hundred milliseconds of patience. Long enough to ride out a burst
		// that clears on its own, short enough that a shed decision is made
		// while the caller still has time to do something about it.
		QueueWait:  100 * time.Millisecond,
		MaxQueued:  4096,
		RetryAfter: time.Second,
	}
}

// Reason names which gate refused, for the metric label and the log line.
//
// A closed set: it becomes a Prometheus label, and one derived from a request
// would be unbounded cardinality.
type Reason string

const (
	ReasonNone       Reason = ""
	ReasonInFlight   Reason = "in_flight"
	ReasonStreams    Reason = "streams"
	ReasonEndpoint   Reason = "endpoint"
	ReasonQueueFull  Reason = "queue_full"
	ReasonCancelled  Reason = "cancelled"
	ReasonShuttingDn Reason = "shutting_down"
)

// Limiter admits or sheds work.
type Limiter struct {
	cfg Config

	global  *gate
	streams *gate

	mu        sync.Mutex
	endpoints map[string]*gate

	shed atomic.Uint64
}

func New(cfg Config) *Limiter {
	d := DefaultConfig()
	if cfg.QueueWait <= 0 {
		cfg.QueueWait = d.QueueWait
	}
	if cfg.MaxQueued <= 0 {
		cfg.MaxQueued = d.MaxQueued
	}
	if cfg.RetryAfter <= 0 {
		cfg.RetryAfter = d.RetryAfter
	}
	return &Limiter{
		cfg:       cfg,
		global:    newGate(cfg.MaxInFlight, cfg.MaxQueued),
		streams:   newGate(cfg.MaxStreams, cfg.MaxQueued),
		endpoints: map[string]*gate{},
	}
}

// Lease is a set of held slots. Release exactly once, on every path.
type Lease struct {
	limiter *Limiter
	held    []*gate
	once    sync.Once
}

// Release returns every slot this lease holds.
func (l *Lease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		for _, g := range l.held {
			g.release()
		}
		l.held = nil
	})
}

// Acquire takes the global slot, and the streaming slot when streaming.
//
// Called before routing, because shedding is only worth doing when it is
// cheaper than the work it avoids — and everything expensive about a request
// happens after this point.
func (l *Limiter) Acquire(ctx context.Context, streaming bool) (*Lease, Reason) {
	if l == nil {
		return &Lease{}, ReasonNone
	}
	lease := &Lease{limiter: l}

	if r := l.take(ctx, lease, l.global, ReasonInFlight); r != ReasonNone {
		lease.Release()
		l.shed.Add(1)
		return nil, r
	}
	if streaming {
		if r := l.take(ctx, lease, l.streams, ReasonStreams); r != ReasonNone {
			lease.Release()
			l.shed.Add(1)
			return nil, r
		}
	}
	return lease, ReasonNone
}

// Endpoint adds this endpoint's slot to an existing lease.
//
// Separate from Acquire because the endpoint is not known until routing has
// run, and routing is not worth doing if the global gate is already full.
func (l *Limiter) Endpoint(ctx context.Context, lease *Lease, id string) Reason {
	if l == nil || lease == nil || l.cfg.MaxPerEndpoint <= 0 || id == "" {
		return ReasonNone
	}
	if r := l.take(ctx, lease, l.gateFor(id), ReasonEndpoint); r != ReasonNone {
		l.shed.Add(1)
		return r
	}
	return ReasonNone
}

func (l *Limiter) take(ctx context.Context, lease *Lease, g *gate, full Reason) Reason {
	if g == nil {
		return ReasonNone
	}
	switch g.acquire(ctx, l.cfg.QueueWait) {
	case admitted:
		lease.held = append(lease.held, g)
		return ReasonNone
	case queueFull:
		return ReasonQueueFull
	case cancelled:
		return ReasonCancelled
	default:
		return full
	}
}

func (l *Limiter) gateFor(id string) *gate {
	l.mu.Lock()
	defer l.mu.Unlock()

	g, ok := l.endpoints[id]
	if !ok {
		g = newGate(l.cfg.MaxPerEndpoint, l.cfg.MaxQueued)
		l.endpoints[id] = g
	}
	return g
}

// RetryAfter is what a shed response should advertise.
func (l *Limiter) RetryAfter() time.Duration {
	if l == nil {
		return 0
	}
	return l.cfg.RetryAfter
}

// Shed counts refused requests, for the metric.
func (l *Limiter) Shed() uint64 {
	if l == nil {
		return 0
	}
	return l.shed.Load()
}

// Stats is the current occupancy, for the admin report.
type Stats struct {
	InFlight    int `json:"in_flight"`
	MaxInFlight int `json:"max_in_flight"`
	Streams     int `json:"streams"`
	MaxStreams  int `json:"max_streams"`
	Waiting     int `json:"waiting"`
	Shed        int `json:"shed_total"`
}

func (l *Limiter) Stats() Stats {
	if l == nil {
		return Stats{}
	}
	return Stats{
		InFlight:    l.global.inUse(),
		MaxInFlight: l.cfg.MaxInFlight,
		Streams:     l.streams.inUse(),
		MaxStreams:  l.cfg.MaxStreams,
		Waiting:     l.global.queued() + l.streams.queued(),
		Shed:        int(l.shed.Load()),
	}
}

type outcome int

const (
	admitted outcome = iota
	timedOut
	queueFull
	cancelled
)

// gate is a counting semaphore with a bounded waiting room.
//
// A buffered channel rather than sync.Cond or a mutex-and-counter: the channel
// gives the fast path a single uncontended send, and it is the only construct
// that composes with a select over the request context and a timer. Waiting is
// counted separately because a channel does not expose how many senders are
// blocked on it, and "how many requests are queued" is precisely what decides
// whether to shed the next one.
type gate struct {
	slots     chan struct{}
	waiting   atomic.Int64
	maxQueued int64
}

func newGate(limit, maxQueued int) *gate {
	if limit <= 0 {
		return nil // disabled
	}
	return &gate{slots: make(chan struct{}, limit), maxQueued: int64(maxQueued)}
}

func (g *gate) acquire(ctx context.Context, wait time.Duration) outcome {
	// Fast path: a free slot, taken without allocating a timer. This is the
	// path essentially every request takes, and it is one channel send.
	select {
	case g.slots <- struct{}{}:
		return admitted
	default:
	}

	// The waiting room is full. Refusing here rather than blocking is what keeps
	// the failure mode "fast 503" instead of "goroutine per waiting request
	// until the process runs out of memory".
	if g.waiting.Add(1) > g.maxQueued {
		g.waiting.Add(-1)
		return queueFull
	}
	defer g.waiting.Add(-1)

	t := time.NewTimer(wait)
	defer t.Stop()

	select {
	case g.slots <- struct{}{}:
		return admitted
	case <-t.C:
		return timedOut
	case <-ctx.Done():
		// The caller left while queued. Not a shed — nobody is waiting for the
		// answer — and counting it as one would make a burst of client
		// cancellations look like Relay refusing work.
		return cancelled
	}
}

func (g *gate) release() {
	if g == nil {
		return
	}
	select {
	case <-g.slots:
	default:
		// Unreachable unless a lease was released twice. Draining an empty
		// channel would block forever, so this refuses rather than deadlocks:
		// an accounting bug must not become a hang.
	}
}

func (g *gate) inUse() int {
	if g == nil {
		return 0
	}
	return len(g.slots)
}

func (g *gate) queued() int {
	if g == nil {
		return 0
	}
	return int(g.waiting.Load())
}
