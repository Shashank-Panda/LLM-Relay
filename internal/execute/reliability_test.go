package execute

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/health"
	"github.com/Shashank-Panda/relay/internal/provider"
)

// --- fault injection harness ---

// scriptedAdapter answers with a queued script, one entry per call.
//
// A script rather than a toggle, because every interesting property here is
// about a *sequence*: fail then succeed, fail twice then move on, fail on one
// endpoint and succeed on another. A stub that returns one fixed answer can
// only test the first call of each.
type scriptedAdapter struct {
	id domain.ProviderID

	steps map[string][]step

	// calls records the endpoints called, in order, so a test can assert on
	// what actually happened rather than only on what came back.
	calls   atomic.Value // []string
	callsMu chan struct{}
}

type step struct {
	err    error
	resp   *provider.Response
	stream provider.Stream
	// delay makes the call take time, for the deadline tests.
	delay time.Duration
}

func newScripted(id domain.ProviderID) *scriptedAdapter {
	a := &scriptedAdapter{id: id, steps: map[string][]step{}, callsMu: make(chan struct{}, 1)}
	a.callsMu <- struct{}{}
	a.calls.Store([]string{})
	return a
}

func (a *scriptedAdapter) script(endpoint string, steps ...step) *scriptedAdapter {
	a.steps[endpoint] = steps
	return a
}

func (a *scriptedAdapter) ID() domain.ProviderID { return a.id }

func (a *scriptedAdapter) next(ep *domain.ModelEndpoint) step {
	<-a.callsMu
	prev := a.calls.Load().([]string)
	n := 0
	for _, c := range prev {
		if c == ep.ID {
			n++
		}
	}
	a.calls.Store(append(append([]string{}, prev...), ep.ID))
	a.callsMu <- struct{}{}

	steps := a.steps[ep.ID]
	if len(steps) == 0 {
		return step{resp: &provider.Response{ID: "ok"}}
	}
	if n >= len(steps) {
		return steps[len(steps)-1]
	}
	return steps[n]
}

func (a *scriptedAdapter) Chat(ctx context.Context, _ *domain.NormalizedRequest, ep *domain.ModelEndpoint, _ provider.Credential) (*provider.Response, error) {
	s := a.next(ep)
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return nil, &provider.Error{Class: provider.ClassCancelled, Err: ctx.Err()}
		}
	}
	if s.err != nil {
		return nil, s.err
	}
	if s.resp == nil {
		return &provider.Response{ID: "ok"}, nil
	}
	return s.resp, nil
}

func (a *scriptedAdapter) ChatStream(ctx context.Context, _ *domain.NormalizedRequest, ep *domain.ModelEndpoint, _ provider.Credential) (provider.Stream, error) {
	s := a.next(ep)
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return nil, &provider.Error{Class: provider.ClassCancelled, Err: ctx.Err()}
		}
	}
	if s.err != nil {
		return nil, s.err
	}
	if s.stream != nil {
		return s.stream, nil
	}
	return &ctxStream{ctx: ctx}, nil
}

func (a *scriptedAdapter) ClassifyError(_ *http.Response, err error) provider.ErrorClass {
	return provider.ClassOf(err)
}

func (a *scriptedAdapter) called() []string { return a.calls.Load().([]string) }

// ctxStream is a stream that stays open until its context ends. It is what
// proves an attempt timeout does not terminate a working response.
type ctxStream struct {
	ctx    context.Context
	sent   int
	closed atomic.Bool
}

func (s *ctxStream) Recv() (*provider.Chunk, error) {
	if s.closed.Load() {
		return nil, io.EOF
	}
	if s.sent == 0 {
		s.sent++
		return &provider.Chunk{Text: "hello"}, nil
	}
	select {
	case <-s.ctx.Done():
		return nil, &provider.Error{Class: provider.ClassCancelled, Err: s.ctx.Err()}
	case <-time.After(20 * time.Millisecond):
		return nil, io.EOF
	}
}

func (s *ctxStream) Usage() *provider.Usage { return &provider.Usage{OutputTokens: 1} }
func (s *ctxStream) Close() error           { s.closed.Store(true); return nil }

func fault(class provider.ErrorClass, msg string) error {
	return &provider.Error{Provider: "openai", Class: class, Message: msg}
}

func ep(id string) *domain.ModelEndpoint {
	return &domain.ModelEndpoint{
		ID: id, Provider: "openai", Model: "m", Deployment: "us",
		CredentialRef: "openai-primary",
		Limits:        domain.Limits{ContextWindow: 128000},
	}
}

func catalog(ids ...string) *domain.Catalog {
	cat := &domain.Catalog{Version: "v1", Endpoints: map[string]*domain.ModelEndpoint{}}
	for _, id := range ids {
		cat.Endpoints[id] = ep(id)
	}
	return cat
}

// ranked builds an optimize-mode decision: the ranking is the failover order,
// because that mode already has the tenant's permission to choose among them.
//
// Failover is populated explicitly rather than derived from Ranked, mirroring
// the router. The two are the same list only in optimize mode — in strict and
// shadow mode Ranked holds a counterfactual the tenant declined to be served,
// and an executor that walked it would make the substitution they refused.
func ranked(chosen string, rest ...string) *domain.Decision {
	d := &domain.Decision{Chosen: chosen, Failover: rest}
	d.Ranked = append(d.Ranked, domain.ScoredCandidate{EndpointID: chosen})
	for _, id := range rest {
		d.Ranked = append(d.Ranked, domain.ScoredCandidate{EndpointID: id})
	}
	return d
}

// testPolicy removes the wall clock from the retry loop. Backoff correctness
// gets its own test; everywhere else, sleeping would only make the suite slow.
func testPolicy() *Policy {
	return &Policy{
		MaxAttempts:           4,
		MaxRetriesPerEndpoint: 2,
		BaseBackoff:           time.Millisecond,
		MaxBackoff:            time.Millisecond,
		AttemptTimeout:        2 * time.Second,
		TotalDeadline:         5 * time.Second,
		Rand:                  func() float64 { return 1 },
		Sleep:                 func(context.Context, time.Duration) error { return nil },
	}
}

func newExecutor(a *scriptedAdapter) *Executor {
	return New(
		provider.NewRegistry(a),
		provider.StaticResolver{"openai-primary": {Ref: "openai-primary"}},
	)
}

// --- the error-class matrix ---

// TestErrorClassBehaviour is the fault-injection table Phase 4 exists to pass.
// Every class produces exactly one action, and getting any of them wrong costs
// either money or a request.
func TestErrorClassBehaviour(t *testing.T) {
	tests := []struct {
		name string
		// first is the fault the primary endpoint returns.
		first error
		// wantCalls is the exact sequence of endpoints called.
		wantCalls []string
		wantErr   bool
	}{
		{
			// Transient and endpoint-specific: back off and try the same one.
			name:      "RetrySame retries the same endpoint",
			first:     fault(provider.ClassRetrySame, "503 from upstream"),
			wantCalls: []string{"a", "a", "a", "b"},
		},
		{
			// This endpoint is unhealthy and the request is fine. Do not waste
			// an attempt proving it twice.
			name:      "RetryOther moves on immediately",
			first:     fault(provider.ClassRetryOther, "provider outage"),
			wantCalls: []string{"a", "b"},
		},
		{
			// The most important class. Retrying a malformed request across
			// three providers produces three bills and one guaranteed failure.
			name:      "Terminal stops everything",
			first:     fault(provider.ClassTerminal, "400 malformed tool schema"),
			wantCalls: []string{"a"},
			wantErr:   true,
		},
		{
			// Nobody is waiting for the answer.
			name:      "Cancelled stops everything",
			first:     fault(provider.ClassCancelled, "client went away"),
			wantCalls: []string{"a"},
			wantErr:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newScripted("openai").
				script("a", step{err: tc.first}).
				script("b", step{resp: &provider.Response{ID: "from-b"}})

			res, err := newExecutor(a).Run(t.Context(), Request{
				Request:  &domain.NormalizedRequest{},
				Catalog:  catalog("a", "b"),
				Decision: ranked("a", "b"),
				Policy:   testPolicy(),
			})

			if tc.wantErr && err == nil {
				t.Fatal("no error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := a.called(); !equal(got, tc.wantCalls) {
				t.Errorf("calls = %v, want %v", got, tc.wantCalls)
			}
			if !tc.wantErr && res.Endpoint.ID != "b" {
				t.Errorf("served by %q, want b", res.Endpoint.ID)
			}
		})
	}
}

func TestRecoveryOnRetry(t *testing.T) {
	a := newScripted("openai").script("a",
		step{err: fault(provider.ClassRetrySame, "hiccup")},
		step{resp: &provider.Response{ID: "recovered"}},
	)

	res, err := newExecutor(a).Run(t.Context(), Request{
		Request: &domain.NormalizedRequest{}, Catalog: catalog("a"),
		Decision: ranked("a"), Policy: testPolicy(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Response.ID != "recovered" {
		t.Errorf("response = %q", res.Response.ID)
	}
	if got := len(res.Attempts); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
	// The distinction the metrics turn on: a retry is the same endpoint again,
	// a failover is a different one. They have different causes and different
	// fixes.
	if got := res.Attempts.Retries(); got != 1 {
		t.Errorf("retries = %d, want 1", got)
	}
	if got := res.Attempts.Failovers(); got != 0 {
		t.Errorf("failovers = %d, want 0", got)
	}
}

func TestAttemptBudgetIsSpendControl(t *testing.T) {
	// Four endpoints all failing, but only three attempts allowed. Every
	// attempt is a real charge at a real provider, so "try harder" and "cost
	// more" are the same sentence.
	a := newScripted("openai")
	for _, id := range []string{"a", "b", "c", "d"} {
		a.script(id, step{err: fault(provider.ClassRetryOther, "down")})
	}

	pol := testPolicy()
	pol.MaxAttempts = 3

	res, err := newExecutor(a).Run(t.Context(), Request{
		Request: &domain.NormalizedRequest{}, Catalog: catalog("a", "b", "c", "d"),
		Decision: ranked("a", "b", "c", "d"), Policy: pol,
	})
	if err == nil {
		t.Fatal("no error with every endpoint failing")
	}
	if got := len(res.Attempts); got != 3 {
		t.Errorf("attempts = %d, want the budget of 3", got)
	}
	if got := len(a.called()); got != 3 {
		t.Errorf("provider was called %d times against a budget of 3", got)
	}
}

func TestRetriesPerEndpointAreBounded(t *testing.T) {
	// A provider that failed twice in a row is not about to succeed on the
	// third try more often than a different provider would on its first.
	a := newScripted("openai").
		script("a", step{err: fault(provider.ClassRetrySame, "still down")}).
		script("b", step{resp: &provider.Response{ID: "from-b"}})

	pol := testPolicy()
	pol.MaxRetriesPerEndpoint = 1

	res, _ := newExecutor(a).Run(t.Context(), Request{
		Request: &domain.NormalizedRequest{}, Catalog: catalog("a", "b"),
		Decision: ranked("a", "b"), Policy: pol,
	})

	if got := a.called(); !equal(got, []string{"a", "a", "b"}) {
		t.Errorf("calls = %v, want two tries at a then b", got)
	}
	if res.Response == nil || res.Response.ID != "from-b" {
		t.Error("did not fail over after exhausting retries")
	}
}

// --- backoff ---

func TestBackoffGrowsAndIsJittered(t *testing.T) {
	p := DefaultPolicy()
	p.BaseBackoff = 100 * time.Millisecond
	p.MaxBackoff = time.Second

	// Full jitter draws from [0, 2^n × base] rather than returning it. The
	// deterministic form synchronizes every client that failed at the same
	// moment into retrying at the same moment, which is how a provider's brief
	// hiccup becomes a sustained outage.
	p.Rand = func() float64 { return 1 }
	for i, want := range []time.Duration{100, 200, 400, 800, 1000, 1000} {
		if got := p.backoff(i, nil); got != want*time.Millisecond {
			t.Errorf("backoff(%d) = %v, want %v", i, got, want*time.Millisecond)
		}
	}

	p.Rand = func() float64 { return 0.5 }
	if got := p.backoff(1, nil); got != 100*time.Millisecond {
		t.Errorf("jittered backoff = %v, want half of 200ms", got)
	}
}

func TestRetryAfterWinsOverComputedBackoff(t *testing.T) {
	p := DefaultPolicy()
	p.BaseBackoff = time.Millisecond
	p.Rand = func() float64 { return 1 }

	err := &provider.Error{Class: provider.ClassRetrySame, RetryAfter: 3 * time.Second}

	// The provider knows when its rate limit clears and Relay is guessing. A
	// gateway that substitutes its own backoff for a stated one is guessing
	// against the truth.
	if got := p.backoff(0, err); got != 3*time.Second {
		t.Errorf("backoff = %v, want the provider's 3s", got)
	}
	// Still bounded, so a hostile or broken Retry-After cannot park a request
	// for an hour inside somebody's deadline.
	p.MaxBackoff = time.Second
	if got := p.backoff(0, err); got != time.Second {
		t.Errorf("backoff = %v, want it capped at MaxBackoff", got)
	}
}

func TestBackoffIsInterruptedByCancellation(t *testing.T) {
	a := newScripted("openai").script("a", step{err: fault(provider.ClassRetrySame, "down")})

	pol := testPolicy()
	pol.Sleep = sleepCtx
	pol.BaseBackoff = 30 * time.Second
	pol.MaxBackoff = 30 * time.Second

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := newExecutor(a).Run(ctx, Request{
		Request: &domain.NormalizedRequest{}, Catalog: catalog("a"),
		Decision: ranked("a"), Policy: pol,
	})
	if err == nil {
		t.Fatal("no error after cancellation")
	}
	// A plain time.Sleep in a retry loop holds a goroutine for its full
	// duration after the client has already hung up — which is how a gateway
	// accumulates goroutines during exactly the incident that produced the
	// retries.
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("waited %v after the client cancelled", d)
	}
}

// --- deadlines ---

func TestTotalDeadlineStopsTheLoop(t *testing.T) {
	a := newScripted("openai").script("a", step{
		err: fault(provider.ClassRetrySame, "slow failure"), delay: 60 * time.Millisecond,
	})

	pol := testPolicy()
	pol.TotalDeadline = 120 * time.Millisecond
	pol.MaxAttempts = 50
	pol.Sleep = sleepCtx
	pol.BaseBackoff = time.Millisecond
	pol.MaxBackoff = time.Millisecond

	start := time.Now()
	res, err := newExecutor(a).Run(t.Context(), Request{
		Request: &domain.NormalizedRequest{}, Catalog: catalog("a"),
		Decision: ranked("a"), Policy: pol,
	})
	if err == nil {
		t.Fatal("no error")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("ran for %v against a 120ms deadline", d)
	}
	if len(res.Attempts) >= 50 {
		t.Error("the attempt budget stopped the loop, not the deadline")
	}
}

func TestAttemptTimeoutDoesNotKillAnOpenStream(t *testing.T) {
	// The subtlety the whole managed-context machinery exists for. A stream
	// legitimately takes minutes; a timeout meant to bound *opening* it must
	// stand down once it is open, or a working response is terminated
	// mid-generation and presents at the client as a truncated answer.
	a := newScripted("openai")

	pol := testPolicy()
	pol.AttemptTimeout = 40 * time.Millisecond

	res, err := newExecutor(a).Run(t.Context(), Request{
		Request: &domain.NormalizedRequest{}, Catalog: catalog("a"),
		Decision: ranked("a"), Streaming: true, Policy: pol,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer res.Stream.Close()

	if _, err := res.Stream.Recv(); err != nil {
		t.Fatalf("first chunk: %v", err)
	}
	// Well past the attempt timeout. The stream must still be alive.
	time.Sleep(80 * time.Millisecond)

	if _, err := res.Stream.Recv(); err != nil && err != io.EOF {
		t.Errorf("stream died after the attempt timeout: %v", err)
	}
}

func TestClosingAStreamReleasesItsContext(t *testing.T) {
	a := newScripted("openai")
	stream := &ctxStream{}
	a.script("a", step{stream: stream})

	res, err := newExecutor(a).Run(t.Context(), Request{
		Request: &domain.NormalizedRequest{}, Catalog: catalog("a"),
		Decision: ranked("a"), Streaming: true, Policy: testPolicy(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Both the attempt context and the total-deadline context are live when a
	// stream is returned. Neither may fire while it is being read, and both
	// must fire when the caller is done — otherwise every completed stream
	// leaks a context and a timer for the life of the process.
	if err := res.Stream.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if !stream.closed.Load() {
		t.Error("Close did not reach the underlying stream")
	}
	// Idempotent, because callers are told to close on every path and therefore
	// sometimes close twice.
	if err := res.Stream.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// --- failover boundary ---

func TestStreamingFailsOverBeforeTheFirstByte(t *testing.T) {
	// ADR-0003's permitted half. The provider refused before any chunk existed,
	// so the client is uncommitted and the switch is invisible to it.
	a := newScripted("openai").
		script("a", step{err: fault(provider.ClassRetryOther, "502 before headers")}).
		script("b", step{})

	res, err := newExecutor(a).Run(t.Context(), Request{
		Request: &domain.NormalizedRequest{}, Catalog: catalog("a", "b"),
		Decision: ranked("a", "b"), Streaming: true, Policy: testPolicy(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer res.Stream.Close()

	if res.Endpoint.ID != "b" {
		t.Errorf("served by %q, want b", res.Endpoint.ID)
	}
	if got := res.Attempts.Failovers(); got != 1 {
		t.Errorf("failovers = %d, want 1", got)
	}
}

func TestNoFailoverOnceAStreamIsOpen(t *testing.T) {
	// ADR-0003's forbidden half, and the property most worth pinning down. Once
	// a stream is open the executor is finished: the client may already have
	// parsed and rendered content, and restarting elsewhere produces duplicated
	// text while splicing produces output no single model generated.
	a := newScripted("openai")
	a.script("a", step{stream: &failingStream{}})
	a.script("b", step{})

	res, err := newExecutor(a).Run(t.Context(), Request{
		Request: &domain.NormalizedRequest{}, Catalog: catalog("a", "b"),
		Decision: ranked("a", "b"), Streaming: true, Policy: testPolicy(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer res.Stream.Close()

	if _, err := res.Stream.Recv(); err != nil {
		t.Fatalf("first chunk: %v", err)
	}
	// The break happens here, after content reached the caller.
	if _, err := res.Stream.Recv(); err == nil {
		t.Fatal("the scripted stream did not break")
	}

	if got := a.called(); !equal(got, []string{"a"}) {
		t.Errorf("calls = %v — the executor failed over after the first byte, which "+
			"would duplicate content the client already rendered", got)
	}
}

// failingStream delivers one chunk and then breaks, which is the mid-stream
// provider failure ADR-0003 accepts as uncovered.
type failingStream struct{ sent bool }

func (s *failingStream) Recv() (*provider.Chunk, error) {
	if !s.sent {
		s.sent = true
		return &provider.Chunk{Text: "partial"}, nil
	}
	return nil, fault(provider.ClassRetrySame, "connection reset mid-stream")
}
func (s *failingStream) Usage() *provider.Usage { return nil }
func (s *failingStream) Close() error           { return nil }

// --- reroute ---

func TestRerouteRefiltersWithACorrectedConstraint(t *testing.T) {
	// A context overflow is neither transient nor permanent: it says the
	// constraint set used for routing was wrong. Retrying is pointless and
	// failing is premature — the right move is to correct the estimate and
	// re-filter, which eliminates the endpoint that just refused and every
	// endpoint too small to do better.
	small, large := ep("small"), ep("large")
	small.Limits.ContextWindow = 8000
	large.Limits.ContextWindow = 200000
	cat := &domain.Catalog{Version: "v1", Endpoints: map[string]*domain.ModelEndpoint{
		"small": small, "large": large,
	}}

	a := newScripted("openai").
		script("small", step{err: fault(provider.ClassReroute, "context_length_exceeded")}).
		script("large", step{resp: &provider.Response{ID: "from-large"}})

	var corrected *domain.NormalizedRequest
	res, err := newExecutor(a).Run(t.Context(), Request{
		Request:  &domain.NormalizedRequest{Estimate: domain.Estimate{InputTokens: 4000}},
		Catalog:  cat,
		Decision: ranked("small", "large"),
		Policy:   testPolicy(),
		Reroute: func(c *domain.NormalizedRequest) (*domain.Decision, error) {
			corrected = c
			return ranked("large"), nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.Response.ID != "from-large" {
		t.Errorf("response = %q", res.Response.ID)
	}
	if !res.Rerouted {
		t.Error("the reroute was not recorded, so the ledger cannot report it")
	}
	if corrected == nil {
		t.Fatal("the router was never re-run")
	}
	// The correction takes the failing endpoint's own window as the new floor:
	// the smallest change consistent with what the provider just measured, and
	// it invents no number nobody observed.
	if got := corrected.Estimate.InputTokens + corrected.Estimate.MaxOutputTokens; got <= small.Limits.ContextWindow {
		t.Errorf("corrected estimate = %d, want above the %d window that just refused it",
			got, small.Limits.ContextWindow)
	}
	if got := a.called(); !equal(got, []string{"small", "large"}) {
		t.Errorf("calls = %v — a reroute must not retry the endpoint that refused", got)
	}
}

func TestRerouteHappensOnce(t *testing.T) {
	// A second reroute would mean the corrected constraint was also wrong, and
	// the loop that follows from believing otherwise is unbounded.
	a := newScripted("openai").
		script("a", step{err: fault(provider.ClassReroute, "context_length_exceeded")}).
		script("b", step{err: fault(provider.ClassReroute, "context_length_exceeded")})

	reroutes := 0
	_, err := newExecutor(a).Run(t.Context(), Request{
		Request: &domain.NormalizedRequest{}, Catalog: catalog("a", "b"),
		Decision: ranked("a", "b"), Policy: testPolicy(),
		Reroute: func(*domain.NormalizedRequest) (*domain.Decision, error) {
			reroutes++
			return ranked("b"), nil
		},
	})
	if err == nil {
		t.Fatal("no error when every candidate rerouted")
	}
	if reroutes > 1 {
		t.Errorf("rerouted %d times, want at most 1", reroutes)
	}
}

func TestRerouteWithoutARouterDegrades(t *testing.T) {
	// Fail open. Without a rerouter the class degrades to "try the next
	// candidate", which is a worse answer than re-filtering and a much better
	// one than failing the request.
	a := newScripted("openai").
		script("a", step{err: fault(provider.ClassReroute, "context_length_exceeded")}).
		script("b", step{resp: &provider.Response{ID: "from-b"}})

	var degradedReasons []string
	e := newExecutor(a).WithDegradedHook(func(_, reason string) {
		degradedReasons = append(degradedReasons, reason)
	})

	res, err := e.Run(t.Context(), Request{
		Request: &domain.NormalizedRequest{}, Catalog: catalog("a", "b"),
		Decision: ranked("a", "b"), Policy: testPolicy(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Response.ID != "from-b" {
		t.Errorf("response = %q, want the next candidate", res.Response.ID)
	}
	// And it is counted, because a fail-open path nobody counts is one nobody
	// knows is being taken (ADR-0010).
	if len(degradedReasons) == 0 {
		t.Error("the degradation was not reported")
	}
}

// --- circuit breaker integration ---

func TestOpenBreakerSkipsAnEndpoint(t *testing.T) {
	tr := health.New(health.Config{MinRequests: 2, FailureRatio: 0.5, OpenFor: time.Minute})

	a := newScripted("openai").
		script("a", step{err: fault(provider.ClassRetrySame, "down")}).
		script("b", step{resp: &provider.Response{ID: "from-b"}})

	e := newExecutor(a).WithTracker(tr)
	in := Request{
		Request: &domain.NormalizedRequest{}, Catalog: catalog("a", "b"),
		Decision: ranked("a", "b"), Policy: testPolicy(),
	}

	// First request trips the breaker on a.
	if _, err := e.Run(t.Context(), in); err != nil {
		t.Fatalf("first run: %v", err)
	}
	before := len(a.called())

	// Second request must not touch it. The breaker is consulted at execution
	// as well as at routing: routing filtered on a snapshot from microseconds
	// ago, and this is what refuses the request behind a half-open probe.
	if _, err := e.Run(t.Context(), in); err != nil {
		t.Fatalf("second run: %v", err)
	}

	for _, c := range a.called()[before:] {
		if c == "a" {
			t.Fatal("called an endpoint whose breaker was open")
		}
	}
}

func TestBreakerIsNotTrippedByCallerErrors(t *testing.T) {
	tr := health.New(health.Config{MinRequests: 1, FailureRatio: 0.5, OpenFor: time.Minute})

	a := newScripted("openai").script("a", step{err: fault(provider.ClassTerminal, "400 bad request")})
	e := newExecutor(a).WithTracker(tr)

	for range 5 {
		e.Run(t.Context(), Request{
			Request: &domain.NormalizedRequest{}, Catalog: catalog("a"),
			Decision: ranked("a"), Policy: testPolicy(),
		})
	}

	// A tenant sending malformed requests must not remove an endpoint from
	// everybody else's routing.
	if tr.Snapshot().For("a").CircuitOpen {
		t.Error("terminal errors tripped the breaker")
	}
}

// --- candidate ordering ---

func TestChosenIsTriedFirstEvenWhenNotRankedFirst(t *testing.T) {
	// In shadow and strict mode the served endpoint is the baseline while
	// Ranked holds the counterfactual ordering. Taking the ranking's head would
	// fail over to the endpoint the tenant declined to be served — turning a
	// provider hiccup into the substitution they explicitly refused.
	// Shadow mode's shape: Chosen is the baseline, Ranked holds the
	// counterfactual ordering, and Failover is empty because there is no
	// equivalent endpoint for the same model.
	d := &domain.Decision{
		Chosen: "baseline",
		Ranked: []domain.ScoredCandidate{{EndpointID: "cheap"}, {EndpointID: "baseline"}},
	}

	a := newScripted("openai").script("baseline", step{resp: &provider.Response{ID: "ok"}})

	res, err := newExecutor(a).Run(t.Context(), Request{
		Request: &domain.NormalizedRequest{}, Catalog: catalog("baseline", "cheap"),
		Decision: d, Policy: testPolicy(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := a.called(); !equal(got, []string{"baseline"}) {
		t.Errorf("calls = %v, want the chosen endpoint only", got)
	}
	if res.Endpoint.ID != "baseline" {
		t.Errorf("served by %q", res.Endpoint.ID)
	}
}

func TestUnconfiguredEndpointDoesNotEndTheRequest(t *testing.T) {
	// A latent outage in a config file: routing can select an endpoint that
	// execution cannot reach. It must cost that endpoint, not the request.
	cat := catalog("a", "b")
	cat.Endpoints["a"].CredentialRef = "missing"

	a := newScripted("openai").script("b", step{resp: &provider.Response{ID: "from-b"}})

	res, err := newExecutor(a).Run(t.Context(), Request{
		Request: &domain.NormalizedRequest{}, Catalog: cat,
		Decision: ranked("a", "b"), Policy: testPolicy(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Response.ID != "from-b" {
		t.Errorf("response = %q", res.Response.ID)
	}
}

func TestErrorNamesWhatWasTried(t *testing.T) {
	a := newScripted("openai")
	for _, id := range []string{"a", "b"} {
		a.script(id, step{err: fault(provider.ClassRetryOther, "down")})
	}

	res, err := newExecutor(a).Run(t.Context(), Request{
		Request: &domain.NormalizedRequest{}, Catalog: catalog("a", "b"),
		Decision: ranked("a", "b"), Policy: testPolicy(),
	})
	if err == nil {
		t.Fatal("no error")
	}
	// The attempt trail survives the failure. Without it "the request failed"
	// is the whole log line, and nobody can tell one dead endpoint from a
	// provider-wide outage.
	if got := len(res.Attempts); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
	if got := res.Attempts.Endpoints(); !equal(got, []string{"a", "b"}) {
		t.Errorf("endpoints tried = %v", got)
	}
	if !errors.As(err, new(*provider.Error)) {
		t.Errorf("err = %v, want it to wrap the last provider error", err)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
