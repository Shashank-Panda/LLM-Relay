package respcache

import (
	"io"
	"testing"
	"time"

	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/provider"
)

func text(s string) domain.ContentPart {
	return domain.ContentPart{Kind: domain.PartText, Text: s}
}

func req() *domain.NormalizedRequest {
	return &domain.NormalizedRequest{
		System:   []domain.ContentPart{text("you are helpful")},
		Messages: []domain.Message{{Role: domain.RoleUser, Parts: []domain.ContentPart{text("hello")}}},
	}
}

func entry(tenant, endpoint, answer string) *Entry {
	return &Entry{
		Tenant:       tenant,
		Endpoint:     endpoint,
		Parts:        []domain.ContentPart{text(answer)},
		FinishReason: provider.FinishStop,
		Usage:        provider.Usage{InputTokens: 100, OutputTokens: 20},
	}
}

// --- key derivation ---

func TestSameRequestSameKey(t *testing.T) {
	// Two separately-built requests with identical content, not the same pointer
	// twice: the guarantee is about equal requests, not about equal objects.
	first, second := req(), req()
	if Key("acme", "ep", first) != Key("acme", "ep", second) {
		t.Error("identical requests produced different keys; an exact-match cache that " +
			"cannot recognise an exact match is an expensive no-op")
	}
}

func TestKeyDependsOnEverythingThatAffectsOutput(t *testing.T) {
	base := Key("acme", "ep", req())

	cases := map[string]func(*domain.NormalizedRequest){
		"system prompt": func(r *domain.NormalizedRequest) {
			r.System = []domain.ContentPart{text("you are terse")}
		},
		"message text": func(r *domain.NormalizedRequest) {
			r.Messages[0].Parts[0].Text = "goodbye"
		},
		"message role": func(r *domain.NormalizedRequest) {
			r.Messages[0].Role = domain.RoleAssistant
		},
		"extra message": func(r *domain.NormalizedRequest) {
			r.Messages = append(r.Messages, domain.Message{
				Role: domain.RoleAssistant, Parts: []domain.ContentPart{text("hi")},
			})
		},
		"tools": func(r *domain.NormalizedRequest) {
			r.Tools = []domain.ToolDef{{Name: "search", Schema: "{}"}}
		},
		"tool choice": func(r *domain.NormalizedRequest) { r.ToolChoice = "required" },
		"response format": func(r *domain.NormalizedRequest) {
			r.ResponseFormat = domain.ResponseFormat{Type: domain.FormatJSONObject}
		},
		"temperature": func(r *domain.NormalizedRequest) {
			r.Params.Temperature = domain.Ptr(0.7)
		},
		"top_p":      func(r *domain.NormalizedRequest) { r.Params.TopP = domain.Ptr(0.9) },
		"max_tokens": func(r *domain.NormalizedRequest) { r.Params.MaxTokens = domain.Ptr(256) },
		"seed":       func(r *domain.NormalizedRequest) { r.Params.Seed = domain.Ptr(int64(7)) },
		"effort": func(r *domain.NormalizedRequest) {
			r.Params.ReasoningEffort = domain.Ptr(domain.EffortHigh)
		},
		"stop": func(r *domain.NormalizedRequest) { r.Params.Stop = []string{"\n\n"} },
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := req()
			mutate(r)
			if Key("acme", "ep", r) == base {
				t.Errorf("changing the %s did not change the key: two requests that "+
					"produce different answers would share one entry", name)
			}
		})
	}
}

func TestKeyDependsOnTenantAndEndpoint(t *testing.T) {
	// The first half of the cross-tenant guarantee. Prompts contain private
	// data, and a hit across tenants is a breach with a nice performance graph.
	if Key("acme", "ep", req()) == Key("globex", "ep", req()) {
		t.Error("two tenants share a cache key")
	}
	// Two models given the same prompt do not give the same answer.
	if Key("acme", "cheap", req()) == Key("acme", "dear", req()) {
		t.Error("two endpoints share a cache key")
	}
}

func TestUnsetIsNotZero(t *testing.T) {
	// A provider's default temperature is not zero, so "unset" and "0" are
	// different requests. An encoding that could not tell them apart would let
	// one answer serve both.
	zero := req()
	zero.Params.Temperature = domain.Ptr(0.0)

	if Key("acme", "ep", zero) == Key("acme", "ep", req()) {
		t.Error("temperature: 0 and an unset temperature share a key")
	}
}

func TestConcatenationCannotCollide(t *testing.T) {
	// Without length-prefixed fields, ("ab","c") and ("a","bc") hash the same,
	// and two different prompts share an entry. This is the specific failure an
	// exact-match cache must be incapable of.
	a := req()
	a.Messages = []domain.Message{{Role: domain.RoleUser, Parts: []domain.ContentPart{text("ab"), text("c")}}}
	b := req()
	b.Messages = []domain.Message{{Role: domain.RoleUser, Parts: []domain.ContentPart{text("a"), text("bc")}}}

	if Key("acme", "ep", a) == Key("acme", "ep", b) {
		t.Error("two different message splits produced the same key")
	}
}

func TestBreakpointsDoNotChangeTheKey(t *testing.T) {
	// A cache marker changes what a request costs, never what it says. Keying on
	// it would make every optimized request miss against its unoptimized twin.
	marked := req()
	marked.System[0].CacheBreakpoint = true

	if Key("acme", "ep", marked) != Key("acme", "ep", req()) {
		t.Error("placing a cache breakpoint changed the response cache key")
	}
}

// --- eligibility ---

func TestCacheableRules(t *testing.T) {
	on := &domain.Route{Cache: domain.RouteCache{Enabled: true}}
	loose := &domain.Route{Cache: domain.RouteCache{Enabled: true, AllowTemperature: true}}

	hot := func() *domain.NormalizedRequest {
		r := req()
		r.Params.Temperature = domain.Ptr(0.9)
		return r
	}

	bypassed := req()
	bypassed.NoCache = true

	tests := []struct {
		name   string
		route  *domain.Route
		req    *domain.NormalizedRequest
		tenant string
		want   Reason
	}{
		{"enabled route", on, req(), "acme", ReasonCacheable},
		{"no route", nil, req(), "acme", ReasonRouteDisabled},
		{"route opted out", &domain.Route{}, req(), "acme", ReasonRouteDisabled},
		{"no tenant to scope to", on, req(), "", ReasonNoTenantOrRoute},
		{"caller asked for variation", on, hot(), "acme", ReasonTemperature},
		{"route allows variation", loose, hot(), "acme", ReasonCacheable},
		{"caller bypassed", on, bypassed, "acme", ReasonNoCacheHeader},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Cacheable(tc.route, tc.req, tc.tenant)
			// ok and the reason are two views of one answer, so they are checked
			// together: a rule that reports "not cacheable" with no reason is one
			// an operator cannot act on.
			if want := tc.want == ReasonCacheable; ok != want {
				t.Fatalf("Cacheable = (%q, %v), want ok=%v", got, ok, want)
			}
			if got != tc.want {
				t.Errorf("reason = %q, want %q", got, tc.want)
			}
		})
	}
}

// --- store ---

func TestGetAndPut(t *testing.T) {
	s := New(Options{})
	s.Put("k", entry("acme", "ep", "answer"), time.Minute)

	got, ok := s.Get("k", "acme", "ep")
	if !ok {
		t.Fatal("stored entry was not returned")
	}
	if got.Parts[0].Text != "answer" {
		t.Errorf("text = %q, want %q", got.Parts[0].Text, "answer")
	}
	if st := s.Stats(); st.Hits != 1 || st.Stores != 1 {
		t.Errorf("stats = %+v, want one hit and one store", st)
	}
}

func TestScopeIsRecheckedOnRead(t *testing.T) {
	// Redundant with the key by design. A SHA-256 collision is not a realistic
	// threat; a bug in key construction is, and its consequence is a
	// cross-tenant prompt disclosure. This is the check that turns that into a
	// miss.
	s := New(Options{})
	s.Put("k", entry("acme", "ep", "private"), time.Minute)

	if _, ok := s.Get("k", "globex", "ep"); ok {
		t.Fatal("another tenant read acme's cached answer")
	}
	if _, ok := s.Get("k", "acme", "other-endpoint"); ok {
		t.Fatal("an entry was served for an endpoint that did not produce it")
	}
	if n := s.Stats().Mismatch; n != 2 {
		t.Errorf("scope_mismatch = %d, want 2 — the counter is how a key-derivation "+
			"bug gets noticed before a customer notices it", n)
	}
}

func TestEntriesExpire(t *testing.T) {
	now := time.Unix(0, 0)
	s := New(Options{Now: func() time.Time { return now }})
	s.Put("k", entry("acme", "ep", "answer"), time.Minute)

	now = now.Add(59 * time.Second)
	if _, ok := s.Get("k", "acme", "ep"); !ok {
		t.Fatal("entry expired early")
	}

	now = now.Add(2 * time.Second)
	if _, ok := s.Get("k", "acme", "ep"); ok {
		t.Fatal("entry served past its TTL")
	}
	if s.Stats().Expired != 1 {
		t.Error("expiry was not counted")
	}
	if s.Stats().Entries != 0 {
		t.Error("the expired entry was not reclaimed")
	}
}

func TestEvictsLeastRecentlyUsed(t *testing.T) {
	s := New(Options{MaxEntries: 2})
	s.Put("a", entry("t", "ep", "a"), time.Minute)
	s.Put("b", entry("t", "ep", "b"), time.Minute)

	// Touching "a" makes "b" the least recently used.
	if _, ok := s.Get("a", "t", "ep"); !ok {
		t.Fatal("a is missing")
	}
	s.Put("c", entry("t", "ep", "c"), time.Minute)

	if _, ok := s.Get("b", "t", "ep"); ok {
		t.Error("evicted the wrong entry: b was least recently used")
	}
	if _, ok := s.Get("a", "t", "ep"); !ok {
		t.Error("evicted a, which had just been read")
	}
}

func TestOversizedEntryIsDeclined(t *testing.T) {
	// An entry larger than the whole budget would evict everything else and then
	// not fit. Declining keeps one pathological request from emptying the cache
	// for every other tenant.
	s := New(Options{MaxBytes: 512})
	s.Put("small", entry("t", "ep", "ok"), time.Minute)

	big := entry("t", "ep", string(make([]byte, 4096)))
	s.Put("big", big, time.Minute)

	if _, ok := s.Get("big", "t", "ep"); ok {
		t.Error("stored an entry larger than the entire budget")
	}
	if _, ok := s.Get("small", "t", "ep"); !ok {
		t.Error("an oversized entry evicted the cache on its way to being rejected")
	}
}

func TestNilStoreIsSafe(t *testing.T) {
	var s *Store
	s.Put("k", entry("t", "ep", "x"), time.Minute)
	if _, ok := s.Get("k", "t", "ep"); ok {
		t.Error("a nil store returned an entry")
	}
	if s.Stats() != (Stats{}) {
		t.Error("a nil store reported statistics")
	}
}

func TestHitRate(t *testing.T) {
	s := New(Options{})
	if got := s.Stats().HitRate(); got != 0 {
		t.Errorf("HitRate on an untouched store = %v, want 0", got)
	}
	s.Put("k", entry("t", "ep", "x"), time.Minute)
	s.Get("k", "t", "ep")
	s.Get("missing", "t", "ep")

	if got := s.Stats().HitRate(); got != 0.5 {
		t.Errorf("HitRate = %v, want 0.5", got)
	}
}

// --- replay and recording ---

func drain(t *testing.T, st provider.Stream) ([]*provider.Chunk, *provider.Usage) {
	t.Helper()
	var out []*provider.Chunk
	for {
		c, err := st.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		out = append(out, c)
	}
	return out, st.Usage()
}

func TestReplayAsStream(t *testing.T) {
	e := &Entry{
		Parts: []domain.ContentPart{
			{Kind: domain.PartText, Text: "the answer"},
			{Kind: domain.PartToolCall, ToolCallID: "call_1", ToolName: "search", Arguments: `{"q":"x"}`},
		},
		FinishReason: provider.FinishToolCalls,
		Usage:        provider.Usage{InputTokens: 10, OutputTokens: 5},
	}

	chunks, usage := drain(t, e.Stream())
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2", len(chunks))
	}
	if chunks[0].Text != "the answer" {
		t.Errorf("text = %q", chunks[0].Text)
	}
	tc := chunks[1].ToolCall
	if tc == nil || tc.Name != "search" || tc.Arguments != `{"q":"x"}` || tc.ID != "call_1" {
		t.Errorf("tool call delta = %+v", tc)
	}
	// The finish reason has to reach the client, and on a stream the only place
	// it can go is the last content-bearing chunk.
	if chunks[1].FinishReason != provider.FinishToolCalls {
		t.Errorf("finish reason = %q, want %q", chunks[1].FinishReason, provider.FinishToolCalls)
	}
	if usage == nil || usage.OutputTokens != 5 {
		t.Errorf("usage = %+v", usage)
	}
}

func TestEmptyAnswerStillCarriesAFinishReason(t *testing.T) {
	e := &Entry{FinishReason: provider.FinishContentFilter}
	chunks, _ := drain(t, e.Stream())
	if len(chunks) != 1 || chunks[0].FinishReason != provider.FinishContentFilter {
		t.Errorf("chunks = %+v, want one carrying the finish reason", chunks)
	}
}

// scriptedStream yields the given chunks, then err.
type scriptedStream struct {
	chunks []*provider.Chunk
	i      int
	err    error
	usage  *provider.Usage
}

func (s *scriptedStream) Recv() (*provider.Chunk, error) {
	if s.i >= len(s.chunks) {
		if s.err != nil {
			return nil, s.err
		}
		return nil, io.EOF
	}
	c := s.chunks[s.i]
	s.i++
	return c, nil
}
func (s *scriptedStream) Usage() *provider.Usage { return s.usage }
func (s *scriptedStream) Close() error           { return nil }

func TestRecorderAssemblesACompletedStream(t *testing.T) {
	src := &scriptedStream{
		chunks: []*provider.Chunk{
			{Text: "hel"},
			{Text: "lo"},
			{ToolCall: &provider.ToolCallDelta{Index: 0, ID: "c1", Name: "search"}},
			{ToolCall: &provider.ToolCallDelta{Index: 0, Arguments: `{"q":`}},
			{ToolCall: &provider.ToolCallDelta{Index: 0, Arguments: `"x"}`}, FinishReason: provider.FinishToolCalls},
		},
		usage: &provider.Usage{InputTokens: 9, OutputTokens: 3},
	}

	var got []domain.ContentPart
	var finish provider.FinishReason
	var usage provider.Usage
	st := Record(src, func(p []domain.ContentPart, f provider.FinishReason, u provider.Usage) {
		got, finish, usage = p, f, u
	})
	drain(t, st)

	if len(got) != 2 {
		t.Fatalf("assembled %d parts, want 2: %+v", len(got), got)
	}
	if got[0].Text != "hello" {
		t.Errorf("text = %q, want %q — fragments must concatenate", got[0].Text, "hello")
	}
	if got[1].Arguments != `{"q":"x"}` {
		t.Errorf("arguments = %q, want %q", got[1].Arguments, `{"q":"x"}`)
	}
	if got[1].ToolName != "search" || got[1].ToolCallID != "c1" {
		t.Errorf("tool identity lost: %+v", got[1])
	}
	if finish != provider.FinishToolCalls {
		t.Errorf("finish = %q", finish)
	}
	if usage.OutputTokens != 3 {
		t.Errorf("usage = %+v", usage)
	}
}

func TestRecorderStoresNothingOnAFailedStream(t *testing.T) {
	// A partial answer in a response cache is the worst possible entry: it looks
	// complete to everyone who reads it afterwards.
	src := &scriptedStream{
		chunks: []*provider.Chunk{{Text: "half an ans"}},
		err:    io.ErrUnexpectedEOF,
	}

	stored := false
	st := Record(src, func([]domain.ContentPart, provider.FinishReason, provider.Usage) {
		stored = true
	})
	for {
		if _, err := st.Recv(); err != nil {
			break
		}
	}
	if stored {
		t.Error("a truncated stream was stored as a complete answer")
	}
}

func TestRecorderStoresNothingForAnEmptyStream(t *testing.T) {
	stored := false
	st := Record(&scriptedStream{}, func([]domain.ContentPart, provider.FinishReason, provider.Usage) {
		stored = true
	})
	drain(t, st)

	if stored {
		t.Error("an empty answer was cached; a provider hiccup would poison the key " +
			"for the whole TTL")
	}
}

func TestRoundTripThroughTheStore(t *testing.T) {
	// The property that makes one entry serve both response shapes: what goes in
	// as a stream comes back out as one.
	s := New(Options{})
	src := &scriptedStream{
		chunks: []*provider.Chunk{{Text: "cached", FinishReason: provider.FinishStop}},
		usage:  &provider.Usage{InputTokens: 4, OutputTokens: 1},
	}
	st := Record(src, func(p []domain.ContentPart, f provider.FinishReason, u provider.Usage) {
		s.Put("k", &Entry{Tenant: "t", Endpoint: "ep", Parts: p, FinishReason: f, Usage: u}, time.Minute)
	})
	drain(t, st)

	e, ok := s.Get("k", "t", "ep")
	if !ok {
		t.Fatal("nothing was stored")
	}
	if resp := e.Response(); len(resp.Parts) != 1 || resp.Parts[0].Text != "cached" {
		t.Errorf("non-streaming replay = %+v", resp.Parts)
	}
	chunks, usage := drain(t, e.Stream())
	if len(chunks) != 1 || chunks[0].Text != "cached" {
		t.Errorf("streaming replay = %+v", chunks)
	}
	if usage.InputTokens != 4 {
		t.Errorf("usage lost: %+v", usage)
	}
}

func TestDefaultTTLApplies(t *testing.T) {
	now := time.Unix(0, 0)
	s := New(Options{Now: func() time.Time { return now }})
	// Zero is not unbounded. A cache with no expiry serves last month's answer
	// to this month's question with no way to notice.
	s.Put("k", entry("t", "ep", "x"), 0)

	now = now.Add(domain.DefaultCacheTTL + time.Second)
	if _, ok := s.Get("k", "t", "ep"); ok {
		t.Error("a zero TTL was treated as unbounded")
	}
}
