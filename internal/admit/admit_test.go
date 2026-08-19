package admit

import (
	"context"
	"sync"
	"testing"
	"time"
)

func tinyConfig() Config {
	return Config{
		MaxInFlight:    2,
		MaxStreams:     1,
		MaxPerEndpoint: 1,
		// Effectively no patience, so the tests measure the shed decision
		// rather than the wait. The queueing behaviour gets its own test.
		QueueWait:  time.Millisecond,
		MaxQueued:  4,
		RetryAfter: 2 * time.Second,
	}
}

func TestAdmitsUpToTheLimit(t *testing.T) {
	l := New(tinyConfig())

	a, r := l.Acquire(t.Context(), false)
	if r != ReasonNone {
		t.Fatalf("first acquire shed: %s", r)
	}
	b, r := l.Acquire(t.Context(), false)
	if r != ReasonNone {
		t.Fatalf("second acquire shed: %s", r)
	}

	if _, r := l.Acquire(t.Context(), false); r != ReasonInFlight {
		t.Errorf("third acquire = %q, want %q", r, ReasonInFlight)
	}

	// A released slot is reusable. Obvious, and the thing a semaphore bug
	// breaks: leaking one slot per request turns the gate into a slow crash.
	a.Release()
	c, r := l.Acquire(t.Context(), false)
	if r != ReasonNone {
		t.Fatalf("acquire after release shed: %s", r)
	}
	b.Release()
	c.Release()

	if got := l.Stats().InFlight; got != 0 {
		t.Errorf("in flight = %d after every release, want 0", got)
	}
}

func TestStreamsAreCountedSeparately(t *testing.T) {
	l := New(tinyConfig())

	s, r := l.Acquire(t.Context(), true)
	if r != ReasonNone {
		t.Fatalf("first stream shed: %s", r)
	}
	defer s.Release()

	// Streams are long-lived and nearly free in CPU while being expensive in
	// file descriptors and goroutines, so a limit sized for non-streaming
	// throughput is far too loose for them.
	if _, r := l.Acquire(t.Context(), true); r != ReasonStreams {
		t.Errorf("second stream = %q, want %q", r, ReasonStreams)
	}
	// And the tighter stream limit must not throttle ordinary traffic: there is
	// still a global slot free.
	n, r := l.Acquire(t.Context(), false)
	if r != ReasonNone {
		t.Errorf("non-streaming request shed by the stream limit: %s", r)
	}
	n.Release()
}

func TestStreamShedReleasesTheGlobalSlot(t *testing.T) {
	l := New(tinyConfig())

	held, _ := l.Acquire(t.Context(), true)
	defer held.Release()

	// The second stream takes a global slot, fails the stream gate, and must
	// give the global one back. Leaking it would let a burst of shed streams
	// exhaust the global gate and shed everything else too — a limiter that
	// makes an overload worse.
	if _, r := l.Acquire(t.Context(), true); r != ReasonStreams {
		t.Fatalf("acquire = %q, want %q", r, ReasonStreams)
	}
	if got := l.Stats().InFlight; got != 1 {
		t.Errorf("in flight = %d, want 1: the shed stream leaked a global slot", got)
	}
}

func TestPerEndpointGate(t *testing.T) {
	l := New(tinyConfig())

	a, _ := l.Acquire(t.Context(), false)
	if r := l.Endpoint(t.Context(), a, "slow@us"); r != ReasonNone {
		t.Fatalf("first endpoint acquire shed: %s", r)
	}
	defer a.Release()

	b, _ := l.Acquire(t.Context(), false)
	defer b.Release()

	// The gate that matters during a partial outage: one slow provider must not
	// absorb every global slot and starve the endpoints that still work.
	if r := l.Endpoint(t.Context(), b, "slow@us"); r != ReasonEndpoint {
		t.Errorf("second call to a busy endpoint = %q, want %q", r, ReasonEndpoint)
	}
	// A different endpoint is unaffected, which is the whole point.
	if r := l.Endpoint(t.Context(), b, "healthy@us"); r != ReasonNone {
		t.Errorf("a healthy endpoint was shed by another's saturation: %s", r)
	}
}

func TestLeaseReleasesEveryGate(t *testing.T) {
	l := New(tinyConfig())

	lease, _ := l.Acquire(t.Context(), true)
	if r := l.Endpoint(t.Context(), lease, "e"); r != ReasonNone {
		t.Fatalf("endpoint acquire shed: %s", r)
	}
	lease.Release()

	// All three gates, from one Release. The handler has exactly one defer, so
	// anything the lease does not return is returned never.
	st := l.Stats()
	if st.InFlight != 0 || st.Streams != 0 {
		t.Errorf("stats = %+v, want everything released", st)
	}
	if r := l.Endpoint(t.Context(), lease, "e"); r != ReasonNone {
		t.Error("the endpoint gate was not released")
	}
}

func TestDoubleReleaseIsSafe(t *testing.T) {
	l := New(tinyConfig())
	lease, _ := l.Acquire(t.Context(), false)

	lease.Release()
	lease.Release()

	// Releasing twice must not return a slot that was never held. Handlers are
	// told to release on every path, so some of them will do it twice — and an
	// over-release inflates the effective limit silently, which is a capacity
	// bug that only appears under the load the gate exists to survive.
	a, _ := l.Acquire(t.Context(), false)
	b, _ := l.Acquire(t.Context(), false)
	if _, r := l.Acquire(t.Context(), false); r != ReasonInFlight {
		t.Errorf("acquire = %q, want %q — a double release raised the limit", r, ReasonInFlight)
	}
	a.Release()
	b.Release()
}

func TestRequestsWaitBrieflyForASlot(t *testing.T) {
	cfg := tinyConfig()
	cfg.QueueWait = 250 * time.Millisecond
	l := New(cfg)

	a, _ := l.Acquire(t.Context(), false)
	b, _ := l.Acquire(t.Context(), false)
	defer b.Release()

	// A burst that clears on its own should be ridden out rather than shed.
	go func() {
		time.Sleep(20 * time.Millisecond)
		a.Release()
	}()

	start := time.Now()
	c, r := l.Acquire(t.Context(), false)
	if r != ReasonNone {
		t.Fatalf("acquire = %q, want it to wait for the slot that was about to free", r)
	}
	defer c.Release()

	if d := time.Since(start); d < 10*time.Millisecond {
		t.Errorf("returned in %v without waiting", d)
	}
}

func TestWaitingIsBounded(t *testing.T) {
	cfg := tinyConfig()
	cfg.QueueWait = 50 * time.Millisecond
	l := New(cfg)

	a, _ := l.Acquire(t.Context(), false)
	b, _ := l.Acquire(t.Context(), false)
	defer a.Release()
	defer b.Release()

	// Nothing frees. The wait must end in a shed rather than in a request that
	// sits until its own deadline expires — a 503 in 50ms is a better answer
	// than a 504 in sixty seconds.
	start := time.Now()
	if _, r := l.Acquire(t.Context(), false); r != ReasonInFlight {
		t.Fatalf("acquire = %q, want %q", r, ReasonInFlight)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("waited %v against a 50ms bound", d)
	}
}

func TestQueueItselfIsBounded(t *testing.T) {
	cfg := tinyConfig()
	cfg.QueueWait = time.Second
	cfg.MaxQueued = 2
	l := New(cfg)

	a, _ := l.Acquire(t.Context(), false)
	b, _ := l.Acquire(t.Context(), false)
	defer a.Release()
	defer b.Release()

	// Fill the waiting room with goroutines that will time out.
	var wg sync.WaitGroup
	for range cfg.MaxQueued {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, r := l.Acquire(t.Context(), false)
			if r == ReasonNone {
				lease.Release()
			}
		}()
	}
	// Give them a moment to block. Without a bound on the queue the limiter is
	// bounded only by how many goroutines the process can hold, which is a
	// limit discovered by running out of memory.
	time.Sleep(50 * time.Millisecond)

	if _, r := l.Acquire(t.Context(), false); r != ReasonQueueFull {
		t.Errorf("acquire = %q, want %q once the waiting room is full", r, ReasonQueueFull)
	}
	wg.Wait()
}

func TestCancelledCallerIsNotAShed(t *testing.T) {
	cfg := tinyConfig()
	cfg.QueueWait = time.Second
	l := New(cfg)

	a, _ := l.Acquire(t.Context(), false)
	b, _ := l.Acquire(t.Context(), false)
	defer a.Release()
	defer b.Release()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	// The caller left while queued. Nobody is waiting for the answer, so this
	// is not Relay refusing work — and counting it as one would make a burst of
	// client cancellations look like an overload.
	if _, r := l.Acquire(ctx, false); r != ReasonCancelled {
		t.Errorf("acquire = %q, want %q", r, ReasonCancelled)
	}
}

func TestZeroLimitsAdmitEverything(t *testing.T) {
	// Phase 1's behaviour: correct for a single-tenant test deployment, and
	// explicitly not something to run in front of production traffic.
	l := New(Config{})
	for range 100 {
		lease, r := l.Acquire(t.Context(), true)
		if r != ReasonNone {
			t.Fatalf("acquire = %q with no limits configured", r)
		}
		lease.Release()
	}
}

func TestNilLimiterAdmits(t *testing.T) {
	var l *Limiter
	lease, r := l.Acquire(context.Background(), true)
	if r != ReasonNone {
		t.Errorf("nil limiter shed: %s", r)
	}
	if r := l.Endpoint(context.Background(), lease, "e"); r != ReasonNone {
		t.Errorf("nil limiter shed an endpoint: %s", r)
	}
	lease.Release()
	if l.Stats() != (Stats{}) {
		t.Error("nil limiter reported stats")
	}
}

func TestShedIsCounted(t *testing.T) {
	l := New(tinyConfig())
	a, _ := l.Acquire(t.Context(), false)
	b, _ := l.Acquire(t.Context(), false)
	defer a.Release()
	defer b.Release()

	for range 3 {
		l.Acquire(t.Context(), false)
	}
	if got := l.Shed(); got != 3 {
		t.Errorf("Shed() = %d, want 3", got)
	}
}

func TestConcurrentAcquireNeverExceedsTheLimit(t *testing.T) {
	const limit = 8
	l := New(Config{MaxInFlight: limit, QueueWait: 10 * time.Millisecond, MaxQueued: 64})

	var (
		mu      sync.Mutex
		inUse   int
		highest int
	)

	var wg sync.WaitGroup
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, r := l.Acquire(context.Background(), false)
			if r != ReasonNone {
				return
			}
			mu.Lock()
			inUse++
			if inUse > highest {
				highest = inUse
			}
			mu.Unlock()

			time.Sleep(time.Millisecond)

			mu.Lock()
			inUse--
			mu.Unlock()
			lease.Release()
		}()
	}
	wg.Wait()

	if highest > limit {
		t.Errorf("peak concurrency %d exceeded the limit of %d", highest, limit)
	}
	if got := l.Stats().InFlight; got != 0 {
		t.Errorf("in flight = %d after everything finished, want 0", got)
	}
}
