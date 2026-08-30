package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Shashank-Panda/relay/internal/admit"
	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/execute"
	"github.com/Shashank-Panda/relay/internal/gateway"
	"github.com/Shashank-Panda/relay/internal/meter"
	"github.com/Shashank-Panda/relay/internal/provider"
	"github.com/Shashank-Panda/relay/internal/routing"
	"github.com/Shashank-Panda/relay/internal/wire"
)

// Disclosure headers.
//
// Substitution is only acceptable because it is visible. These make it visible
// on every affected response rather than in a dashboard the caller has to go
// looking for — see ADR-0007.
const (
	HeaderPin         = "X-Relay-Pin"
	HeaderEndpoint    = "X-Relay-Endpoint"
	HeaderBaseline    = "X-Relay-Baseline"
	HeaderMode        = "X-Relay-Mode"
	HeaderSubstituted = "X-Relay-Substituted"
	HeaderCatalog     = "X-Relay-Catalog-Version"

	// HeaderDecision is a compact summary of why this endpoint was chosen.
	// Bounded deliberately: the full Decision belongs in the ledger and in a
	// dry-run response, not in a header that proxies may truncate.
	HeaderDecision = "X-Relay-Decision"

	// HeaderSaved is what this request saved, in USD.
	//
	// On a non-streaming response it comes from provider-reported actuals. On a
	// stream it cannot: headers are fixed before the first byte and the token
	// counts arrive at the end. Streaming therefore carries the pre-flight
	// estimate and sets HeaderSavedEstimated so the two are never confused.
	HeaderSaved          = "X-Relay-Saved-Usd"
	HeaderSavedEstimated = "X-Relay-Saved-Estimated"

	// HeaderCost and HeaderBaselineCost are the two numbers HeaderSaved is the
	// difference of.
	//
	// Emitted together with it and under exactly the same conditions, because a
	// saving without its operands is a claim rather than a measurement — the
	// caller cannot check the subtraction, and checking it is the entire reason
	// this product reports both costs on every request. HeaderSavedEstimated
	// qualifies all three at once: on a stream they are pre-flight estimates.
	//
	// HeaderCost is zero on a response-cache hit. No tokens were bought, so the
	// honest cost is nothing and the whole baseline is the saving.
	HeaderCost         = "X-Relay-Cost-Usd"
	HeaderBaselineCost = "X-Relay-Baseline-Usd"

	// HeaderShadowSaved is what optimize mode would have saved. Present only in
	// shadow mode, and never merged with HeaderSaved: one is money saved, the
	// other is money that could have been.
	HeaderShadowSaved = "X-Relay-Shadow-Saved-Usd"

	// HeaderOptimizations lists the levers applied to this request, as
	// "lever=before>after" pairs.
	//
	// Present on every optimized response, not on request. An optimization the
	// customer cannot see is indistinguishable from a bug, and "shorter than
	// yesterday's answer" is a support ticket this header answers on its own
	// (ADR-0008).
	HeaderOptimizations = "X-Relay-Optimizations"

	// HeaderCache reports the response cache: hit, miss, or off with a reason.
	HeaderCache = "X-Relay-Cache"

	// HeaderNoCache lets a caller bypass the response cache for one request.
	//
	// Spelled as a request header rather than a body field because the body is
	// the OpenAI schema and adding to it would break client libraries — the same
	// constraint that makes the model string carry routing intent.
	HeaderNoCache = "X-Relay-No-Cache"

	// HeaderCredential carries one caller-supplied provider key, as
	// "<credential_ref> <secret>". Repeatable: send it once per ref.
	//
	// Distinct from Authorization, which is a *Relay tenant* key and not a
	// provider key. The two authenticate different things to different parties
	// and are never interchangeable.
	//
	// A key arriving this way is used for this request and is never stored,
	// never cached, never logged, and never written to the savings ledger.
	HeaderCredential = "X-Relay-Credential"

	// HeaderAssumeCredentials makes a DRY RUN reason about routing as though
	// every credential in the catalog were configured.
	//
	// It exists so the explanation is legible on a machine holding no keys —
	// which is every machine that has just cloned this repository, and the
	// audience the explanation is most valuable to. Without it, the moment
	// routing began eliminating endpoints for want of a credential, a keyless
	// dry run would collapse from a full ranking to a single row and stop
	// demonstrating anything.
	//
	// Honoured only on a dry run, and ignored everywhere else. A live request
	// that could be talked into routing to an endpoint Relay cannot
	// authenticate to would be a denial of service with a polite name.
	HeaderAssumeCredentials = "X-Relay-Assume-Credentials"

	// HeaderDryRun asks what Relay would do, without doing it. Answered with the
	// full decision and the optimizations, and no provider is contacted.
	HeaderDryRun = "X-Relay-Dry-Run"

	// HeaderAttempts is how many provider calls this answer took, present only
	// when it took more than one.
	//
	// Disclosed because a retried request is slower and dearer than a clean one,
	// and a caller debugging their own latency should not have to guess whether
	// the extra second was the model thinking or Relay recovering.
	HeaderAttempts = "X-Relay-Attempts"

	// HeaderRerouted marks a request the router re-decided mid-flight after a
	// provider rejected its routing constraints.
	HeaderRerouted = "X-Relay-Rerouted"

	// HeaderFailover marks a request served by a different endpoint running the
	// same model — a recovery, not a substitution. Kept apart from
	// HeaderSubstituted because conflating them would report a region failover
	// as a model downgrade.
	HeaderFailover = "X-Relay-Failover"

	// HeaderEscalated marks a request where a downgraded model produced invalid
	// output and the baseline was retried (ADR-0009).
	//
	// Disclosed because the caller paid for two answers. It is also the honest
	// counterpart to the savings header on the same response: X-Relay-Saved-Usd
	// will be *negative* here, and a caller who sees the cost without the reason
	// has been given half the story.
	HeaderEscalated = "X-Relay-Escalated"
)

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	r.Body = http.MaxBytesReader(w, r.Body, s.opts.MaxBodyBytes)

	var body wire.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			wire.WriteError(w, http.StatusRequestEntityTooLarge,
				"request body exceeds the maximum size", wire.TypeInvalidRequest, "")
			return
		}
		wire.WriteError(w, http.StatusBadRequest,
			"request body is not valid JSON: "+err.Error(), wire.TypeInvalidRequest, "")
		return
	}

	req, err := wire.Decode(&body)
	if err != nil {
		wire.WriteDecodeError(w, err)
		return
	}
	req.ID = RequestID(r.Context())

	// Admission before routing, because shedding is only worth doing when it is
	// cheaper than the work it avoids — and everything expensive about a
	// request happens after this point. A fast 503 the caller can retry beats a
	// slow 504 for work Relay already paid a provider to perform.
	lease, shed := s.admit.Acquire(r.Context(), body.Stream)
	if shed != admit.ReasonNone {
		s.writeShed(w, shed)
		return
	}
	defer lease.Release()

	tn := tenantOf(r.Context(), s.tenants)

	if truthy(r.Header.Get(HeaderNoCache)) {
		// Suppressed before Prepare rather than checked at lookup, so that one
		// flag turns off both the read and the write. A bypass that still
		// populated the cache would let a caller asking for a fresh answer
		// decide what everyone else gets served.
		req.NoCache = true
	}

	// A dry run may be asked to reason as though every credential existed, so
	// that the routing arithmetic is visible on a machine holding no keys. The
	// gate is here, in the handler, and not in the gateway: a live request that
	// could be talked into routing to an endpoint Relay cannot authenticate to
	// would turn an explanation feature into a self-inflicted outage.
	dry := truthy(r.Header.Get(HeaderDryRun))
	assume := dry && assumesCredentials(r.Header.Get(HeaderAssumeCredentials))

	keys, err := parseCredentials(r.Header)
	if err != nil {
		wire.WriteError(w, http.StatusBadRequest, err.Error(),
			wire.TypeInvalidRequest, "invalid_credential_header")
		return
	}
	if keys != nil && insecureCredentials(r, s.opts.AllowInsecureCredentials) {
		// Refused rather than warned about. The request would otherwise
		// succeed, so nothing downstream would ever mention that a live
		// provider key had just crossed the network in the clear.
		wire.WriteError(w, http.StatusBadRequest,
			"refusing to accept "+HeaderCredential+" over a plaintext connection; "+
				"use HTTPS, or start the gateway with -allow-insecure-credentials "+
				"if this is a local deployment",
			wire.TypeInvalidRequest, "insecure_credential")
		return
	}

	cat := s.store.Current()
	tid := ""
	if tn != nil {
		tid = tn.ID
	}
	prepared, err := s.gw.Prepare(req, tn, gateway.PrepareOptions{
		Model:              body.Model,
		Pin:                r.Header.Get(HeaderPin),
		Credentials:        s.gw.CredentialSnapshot(r.Context(), cat, tid, resolverOf(keys)),
		RequestCredentials: keys,
		AssumeCredentials:  assume,
	})
	if err != nil {
		s.writePrepareError(w, err, cat)
		return
	}

	// The per-endpoint gate needs the endpoint, so it cannot run with the
	// global one. It is the gate that matters during a partial outage: without
	// it, one slow provider absorbs every global slot and starves the endpoints
	// that are still healthy.
	if shed := s.admit.Endpoint(r.Context(), lease, prepared.Decision.Chosen); shed != admit.ReasonNone {
		s.writeShed(w, shed)
		return
	}

	s.observeOptimize(prepared)
	s.setDisclosureHeaders(w, prepared)

	if dry {
		// Answered before any provider contact and before any ledger record:
		// nothing happened, so nothing is billed and nothing is counted.
		writeJSON(w, http.StatusOK, dryRun(prepared))
		return
	}

	if req.Stream {
		s.streamCompletion(w, r, prepared, start)
		return
	}
	s.chatCompletion(w, r, prepared, start)
}

// writeExecuteError reports a provider-call failure, with one special case.
//
// A missing credential reaches here rather than writePrepareError whenever
// routing had *something* to serve — a route fallback, or strict mode, which
// skips filtering entirely — and only the executor discovered there was no key
// for it. The resolver's own message is correct but partial: it names the
// environment variable, which is the right remedy for the operator of a
// self-hosted install and useless advice to somebody using a hosted console,
// where the machine holding that environment is not theirs.
//
// So the transport's own remedy is added here, in the layer that knows the
// header exists. This is the most likely first-run failure for anyone who has
// just started the gateway, and it is worth answering completely.
func (s *Server) writeExecuteError(w http.ResponseWriter, err error) {
	var missing *provider.ErrNoCredential
	if errors.As(err, &missing) {
		// Built from the credential error itself rather than from err.Error():
		// the executor's wrapper reports which candidates were tried and how
		// many attempts it took, which is exactly what an operator wants in a
		// log and exactly what a caller does not need in front of the one
		// sentence telling them what to do about it.
		msg := missing.Error() + "; or send " + HeaderCredential + ": " + missing.Ref + " <key>"
		wire.WriteError(w, http.StatusUnauthorized, msg,
			wire.TypeAuthentication, "no_credential")
		return
	}
	wire.WriteProviderError(w, err)
}

// assumesCredentials reads X-Relay-Assume-Credentials.
//
// "all" is the only value that does anything. Spelled as a word rather than a
// boolean so that the request says what it is asking for, and so "available" —
// the default — can be written down explicitly by a caller who wants to be sure
// they are seeing their real candidate set.
func assumesCredentials(v string) bool {
	return strings.EqualFold(strings.TrimSpace(v), "all")
}

// writePrepareError reports a failure that happened before any provider was
// contacted.
func (s *Server) writePrepareError(w http.ResponseWriter, err error, cat *domain.Catalog) {
	var unknown *gateway.ErrUnknownModel
	if errors.As(err, &unknown) {
		body := wire.ErrorResponse{Error: wire.ErrorBody{
			Message: unknown.Error(),
			Type:    wire.TypeInvalidRequest,
			Code:    "model_not_found",
			Param:   "model",
		}}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(body)
		return
	}

	// Every candidate eliminated for want of a credential is a caller problem
	// with a one-header fix, not a server problem to wait out. Reported as 401
	// with the missing refs named, because the alternative — the flat 503 below
	// — is the single most likely first-run experience for someone who has just
	// started the gateway, and it tells them nothing they can act on.
	//
	// "Every", not "any": a request where one endpoint lacked a key and the rest
	// failed a quality floor is not a credentials problem, and saying so would
	// send the caller after the wrong thing.
	var nc *routing.NoCandidateError
	if errors.As(err, &nc) && nc.AllRejectedFor(domain.RejectNoCredential) {
		msg := "no provider credential is available for this request"
		if refs := nc.MissingRefs(cat); len(refs) > 0 {
			msg += " (missing: " + strings.Join(refs, ", ") + ")"
		}
		msg += "; send X-Relay-Credential: <ref> <key>, or set the matching " +
			"RELAY_CRED_* variable, or use X-Relay-Dry-Run with " +
			"X-Relay-Assume-Credentials: all to see the routing decision without one"
		wire.WriteError(w, http.StatusUnauthorized, msg,
			wire.TypeAuthentication, "no_credential")
		return
	}

	// No viable candidate and nothing to fall back to. A configuration problem
	// rather than a caller problem, so it reports as 503: retrying later may
	// well work, retrying the request unchanged right now will not.
	if errors.Is(err, routing.ErrNoCandidate) {
		wire.WriteError(w, http.StatusServiceUnavailable,
			"no endpoint is currently able to serve this request",
			wire.TypeAPIError, string(provider.ClassRetryOther))
		return
	}

	wire.WriteError(w, http.StatusInternalServerError,
		err.Error(), wire.TypeAPIError, "")
}

// setDisclosureHeaders must run before any body is written.
//
// After the first byte of a stream the headers are already on the wire, which
// is why Prepare is separate from execution: the decision has to exist before
// the response starts, or substitution could not be disclosed on the requests
// where it matters most.
func (s *Server) setDisclosureHeaders(w http.ResponseWriter, p *gateway.Prepared) {
	d := p.Decision
	h := w.Header()

	h.Set(HeaderEndpoint, d.Chosen)
	h.Set(HeaderCatalog, d.CatalogVersion)
	if d.Baseline.EndpointID != "" {
		h.Set(HeaderBaseline, d.Baseline.EndpointID)
	}
	if d.Baseline.Mode != "" {
		h.Set(HeaderMode, string(d.Baseline.Mode))
	}
	if d.Substituted() {
		// Mandatory, not optional, whenever the served endpoint differs from
		// what the caller named.
		h.Set(HeaderSubstituted, "true")
	}
	h.Set(HeaderDecision, decisionSummary(d))

	if len(d.Optimizations) > 0 {
		h.Set(HeaderOptimizations, optimizationSummary(d.Optimizations))
	}
	// Reported before the lookup happens, so a stream — whose headers are fixed
	// before its first byte — still says something true. A hit upgrades this to
	// "hit" while the response is still headers-only; anything left at "miss"
	// was one.
	if p.CacheKey != "" {
		h.Set(HeaderCache, "miss")
	} else if p.CacheSkip != "" {
		h.Set(HeaderCache, "off; "+string(p.CacheSkip))
	}
}

// optimizationSummary renders the applied levers for a header.
//
// Names and values, no reasons. Same constraint as the decision summary: a
// header is a bounded medium, and the reasons are in the dry-run response and
// the completion log where there is room for them.
func optimizationSummary(ops []domain.Optimization) string {
	parts := make([]string, 0, len(ops))
	for _, o := range ops {
		parts = append(parts, o.String())
	}
	return strings.Join(parts, ", ")
}

// observeOptimize publishes the optimizer's latency and any fail-open it took.
//
// Recorded for every request including the ones where no lever was enabled,
// because the histogram answers "what does the optimizer cost us" and a sample
// set drawn only from the passes that did work answers a different question.
func (s *Server) observeOptimize(p *gateway.Prepared) {
	if s.metrics == nil {
		return
	}
	s.metrics.ObserveOptimize(p.Optimize)
}

// observeCacheOutcome counts a cacheable request that found no entry.
//
// Called after the lookup rather than beside it, because "cacheable" and "miss"
// are different facts and only one of them is known before execution. Hits are
// counted from the ledger record instead, where the tenant and route are already
// on hand — so the two counters come from the two places that actually know.
func (s *Server) observeCacheOutcome(p *gateway.Prepared) {
	if s.metrics == nil || p.CacheKey == "" || p.CacheHit {
		return
	}
	s.metrics.ObserveCacheMiss(p.TenantID(), p.Decision.RouteName)
}

// setServedHeaders corrects the pre-flight disclosure once the answer's origin
// is known.
//
// The headers have to be written before execution — on a stream they are fixed
// the moment the first frame goes out, which is why the decision exists before
// the response starts at all. But a retry or a failover can move the endpoint
// after that, and a disclosure header naming the endpoint Relay *intended* to
// use is worse than none: it is a wrong answer to "which model produced this",
// stated with confidence.
//
// Safe on both paths because it runs after execution and before the first byte:
// non-streaming has written nothing yet, and streaming has only opened the
// upstream connection.
func setServedHeaders(w http.ResponseWriter, p *gateway.Prepared) {
	h := w.Header()

	if p.CacheHit {
		h.Set(HeaderCache, "hit")
	}
	if p.Decision.Chosen != "" {
		h.Set(HeaderEndpoint, p.Decision.Chosen)
	}
	// Substitution is only acceptable because it is visible, and a failover
	// across models is a substitution however it came about. A failover to
	// another deployment of the *same* model is not one, and saying so would
	// tell a caller they got a cheaper model when they got the one they asked
	// for from a different region.
	if p.SubstitutedModel() {
		h.Set(HeaderSubstituted, "true")
	} else {
		h.Del(HeaderSubstituted)
	}
	if p.FailedOver() {
		h.Set(HeaderFailover, "true")
	}
	// Re-rendered because Chosen may have moved: the summary written before
	// execution names the endpoint Relay intended to use.
	h.Set(HeaderDecision, decisionSummary(p.Decision))

	if n := len(p.Attempts); n > 1 {
		h.Set(HeaderAttempts, strconv.Itoa(n))
	}
	if p.Rerouted {
		h.Set(HeaderRerouted, "true")
	}
	if e := p.Escalation; e != nil {
		h.Set(HeaderEscalated, string(e.Reason))
	}
}

// writeShed refuses a request the gateway has no capacity for.
//
// 503 with Retry-After, never 429. The distinction is not pedantry: 429 says
// "you sent too much", which blames a caller who may have sent one request, and
// SDK retry logic treats the two differently. This is Relay saying it is busy,
// which is what 503 means.
//
// Nothing is recorded in the ledger. No provider was called, nothing was spent,
// and a shed request in the savings report would dilute every per-request figure
// with work that never happened.
func (s *Server) writeShed(w http.ResponseWriter, reason admit.Reason) {
	if reason == admit.ReasonCancelled {
		// The caller left while queued. Writing a response to a closed
		// connection is pointless, and counting it as a shed would make a burst
		// of client cancellations look like Relay refusing work.
		return
	}
	if s.metrics != nil {
		s.metrics.Shed.WithLabelValues(string(reason)).Inc()
	}
	if d := s.admit.RetryAfter(); d > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(d.Seconds()+0.5)))
	}
	wire.WriteError(w, http.StatusServiceUnavailable,
		"relay is at capacity; retry shortly", wire.TypeAPIError, string(provider.ClassRetrySame))
}

// truthy reads a boolean request header.
//
// Permissive on purpose. These headers are typed by hand into curl and into
// client config, and "1", "true", and "yes" all obviously mean the same thing;
// rejecting two of the three would make the feature look broken. An unset or
// unrecognised value is false, so the default is always the unmodified
// behaviour.
func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// decisionSummary renders the ranking compactly enough for a header.
//
// Top three only, and no reasons. A header is a bounded medium — proxies vary,
// and an 8KB limit is common — so this is the summary that says *what* was
// decided; the ledger and the dry-run endpoint carry the full explanation.
func decisionSummary(d *domain.Decision) string {
	type entry struct {
		Endpoint string  `json:"endpoint"`
		Total    float64 `json:"total"`
	}
	out := struct {
		Route          string  `json:"route,omitempty"`
		Chosen         string  `json:"chosen"`
		Counterfactual string  `json:"counterfactual,omitempty"`
		Mode           string  `json:"mode"`
		Ranked         []entry `json:"ranked,omitempty"`
		Rejected       int     `json:"rejected,omitempty"`
		Fallback       bool    `json:"fallback,omitempty"`
	}{
		Route:          d.RouteName,
		Chosen:         d.Chosen,
		Counterfactual: d.Counterfactual,
		Mode:           string(d.Baseline.Mode),
		Rejected:       len(d.Rejected),
		Fallback:       d.UsedFallback,
	}
	for i, c := range d.Ranked {
		if i == 3 {
			break
		}
		out.Ranked = append(out.Ranked, entry{c.EndpointID, round3(c.Total)})
	}

	buf, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(buf)
}

func round3(f float64) float64 { return float64(int(f*1000+0.5)) / 1000 }

// setSavingsHeaders reports the money figures once they are known.
func (s *Server) setSavingsHeaders(w http.ResponseWriter, rec *meter.Record, estimated bool) {
	if !rec.SavingMeasured {
		return
	}
	h := w.Header()
	h.Set(HeaderSaved, usd(rec.Saved))
	h.Set(HeaderCost, usd(rec.Cost))
	h.Set(HeaderBaselineCost, usd(rec.BaselineCost))
	if estimated {
		// The caller must be able to tell a measured figure from a pre-flight
		// one. A dashboard that summed both would be summing guesses.
		h.Set(HeaderSavedEstimated, "true")
	}
	if rec.Counterfactual != "" {
		h.Set(HeaderShadowSaved, usd(rec.ShadowSaved))
	}
}

func usd(m domain.Money) string {
	return strconv.FormatFloat(m.Dollars(), 'f', 6, 64)
}

func (s *Server) chatCompletion(w http.ResponseWriter, r *http.Request, p *gateway.Prepared, start time.Time) {
	rec := p.NewRecord(false)
	rec.At = start

	providerStart := time.Now()
	resp, _, att, err := s.gw.Chat(r.Context(), p)
	rec.ProviderDuration = time.Since(providerStart)

	if err != nil {
		p.RecordAttempts(&rec)
		rec.Outcome = outcomeFor(err)
		rec.ErrorClass = string(provider.ClassOf(err))
		rec.Duration = time.Since(start)
		s.record(rec)

		s.logAttempt(r, p, att, nil)
		s.writeExecuteError(w, err)
		return
	}

	s.observeCacheOutcome(p)
	p.RecordAttempts(&rec)
	setServedHeaders(w, p)
	p.Price(&rec, resp.Usage)
	rec.FinishReason = string(resp.FinishReason)
	rec.Outcome = meter.OutcomeSuccess
	rec.Duration = time.Since(start)

	// Non-streaming can report actuals, because nothing has been written yet.
	s.setSavingsHeaders(w, &rec, false)

	s.record(rec)
	s.logCompletion(r, p, rec)

	writeJSON(w, http.StatusOK, wire.EncodeResponse(resp, p.RequestedModel, time.Now().Unix()))
}

// streamCompletion relays a provider stream as SSE.
//
// Three properties this function exists to guarantee:
//
//   - the stream is closed on every path, including the panic the recovery
//     middleware catches, because a leaked stream reader holds a connection and
//     a goroutine until the process dies;
//   - no goroutine is spawned per stream, so there is nothing that can outlive
//     the request context;
//   - a client disconnect propagates upstream, because r.Context() is what the
//     provider call was built from.
func (s *Server) streamCompletion(w http.ResponseWriter, r *http.Request, p *gateway.Prepared, start time.Time) {
	rec := p.NewRecord(true)
	rec.At = start

	// The estimate is all that can be known before the first byte, and the
	// header must be written now or not at all.
	s.setEstimatedSavings(w, p)

	providerStart := time.Now()
	stream, _, att, err := s.gw.Stream(r.Context(), p)
	if err != nil {
		p.RecordAttempts(&rec)
		rec.ProviderDuration = time.Since(providerStart)
		rec.Duration = time.Since(start)
		rec.Outcome = outcomeFor(err)
		rec.ErrorClass = string(provider.ClassOf(err))
		s.record(rec)

		s.logAttempt(r, p, att, nil)
		// Nothing has been written yet, so this is still a normal JSON error
		// with a real status code. After the first frame it could not be.
		s.writeExecuteError(w, err)
		return
	}
	defer stream.Close()

	s.observeCacheOutcome(p)
	p.RecordAttempts(&rec)
	setServedHeaders(w, p)

	// A cache hit knows its exact token counts before the first byte, which a
	// live stream never does. Replacing the pre-flight estimate with the real
	// figure — and clearing the flag that said it was one — is the whole reason
	// this runs after the lookup and before the writer starts.
	if p.CacheHit {
		cached := p.NewRecord(true)
		p.Price(&cached, p.CachedUsage)
		w.Header().Del(HeaderSavedEstimated)
		s.setSavingsHeaders(w, &cached, false)
	}

	s.streamsOpened()
	defer s.streamsClosed()

	sw := wire.NewStreamWriter(w)
	id := "chatcmpl-" + p.Request.ID
	created := time.Now().Unix()

	finish := func(outcome meter.Outcome, err error) {
		p.RecordAttempts(&rec)
		rec.ProviderDuration = time.Since(providerStart)
		rec.Duration = time.Since(start)
		rec.Outcome = outcome
		if err != nil {
			rec.ErrorClass = string(provider.ClassOf(err))
			// Past the first token no failover is honest (ADR-0003), so this
			// failure is the residual risk of that decision rather than an
			// ordinary error. Counted separately, because the ADR says the
			// choice gets revisited with data if the gap turns out larger than
			// expected — and this is the data.
			rec.StreamFailedAfterTTFT = rec.TTFT > 0
		}
		s.record(rec)
	}

	// The opening frame carries only role:"assistant". SDKs that build a
	// message incrementally use it to initialise the object; without it the
	// first content delta is applied to nothing.
	if err := sw.Send(wire.RoleChunk(id, p.RequestedModel, created)); err != nil {
		finish(meter.OutcomeCancelled, err)
		s.logAttempt(r, p, att, nil)
		return
	}

	// Provider chunks arrive on a channel so the loop can also wake on a
	// heartbeat tick. stream.Recv blocks, and there is no other way to
	// interleave a blocked read with a timer.
	//
	// The goroutine cannot outlive this function. It parks on either Recv or
	// the send below; recvDone releases the second and the caller's deferred
	// stream.Close releases the first. Both defers run, and because defers are
	// LIFO and recvDone is registered last, it is signalled before the stream
	// is closed rather than after.
	type recvResult struct {
		chunk *provider.Chunk
		err   error
	}
	recvCh := make(chan recvResult)
	recvDone := make(chan struct{})
	defer close(recvDone)

	go func() {
		for {
			c, err := stream.Recv()
			select {
			case recvCh <- recvResult{chunk: c, err: err}:
			case <-recvDone:
				return
			}
			if err != nil {
				return
			}
		}
	}()

	// Heartbeat comment frames stop an intermediary closing a connection while
	// a model is still thinking. Without them a reasoning model that takes
	// ninety seconds before its first token is indistinguishable, to every
	// proxy between here and the caller, from a dead connection — and the
	// console adds two such hops.
	//
	// A ticker rather than a timer reset after every frame: an extra comment
	// during active generation costs one line of text that every SSE client
	// already ignores, and stopping-draining-resetting a timer around a channel
	// that may have already fired is a well-known source of subtle bugs. The
	// cheap imprecision is the better trade.
	var beat <-chan time.Time
	if hb := s.opts.StreamHeartbeat; hb > 0 {
		t := time.NewTicker(hb)
		defer t.Stop()
		beat = t.C
	}

	var firstToken time.Time
	for {
		var res recvResult
		select {
		case res = <-recvCh:
		case <-beat:
			if err := sw.Comment("keep-alive"); err != nil {
				// Same meaning as a failed Send below: the client is gone.
				finish(meter.OutcomeCancelled, err)
				s.logDisconnect(r, p, att)
				return
			}
			continue
		}

		chunk, err := res.chunk, res.err
		if err == io.EOF {
			break
		}
		if err != nil {
			finish(outcomeFor(err), err)
			s.logAttempt(r, p, att, err)
			// Past the first byte the status code is already sent, so the only
			// honest way to report this is inside the stream. Closing the
			// connection instead is indistinguishable at the client from a
			// network fault, and a client that cannot tell them apart retries a
			// request guaranteed to fail again.
			sw.Error(providerMessage(err), wire.TypeAPIError, string(provider.ClassOf(err)))
			return
		}

		if firstToken.IsZero() && (chunk.Text != "" || chunk.ToolCall != nil) {
			firstToken = time.Now()
			rec.TTFT = firstToken.Sub(start)
			// The endpoint's latency signal, reported here because the executor
			// is long gone: it returned when the stream opened, and this is the
			// first moment the model has actually said anything.
			s.gw.ObserveTTFT(p, rec.TTFT)
		}
		if chunk.FinishReason != "" {
			rec.FinishReason = string(chunk.FinishReason)
		}

		if err := sw.Send(wire.EncodeChunk(chunk, id, p.RequestedModel, created)); err != nil {
			// The client is gone. Returning cancels r.Context(), which aborts
			// the upstream call and stops the meter.
			finish(meter.OutcomeCancelled, err)
			s.logDisconnect(r, p, att)
			return
		}
	}

	usage := provider.Usage{}
	if u := stream.Usage(); u != nil {
		usage = *u
	}

	if !usage.Estimated {
		if err := sw.Send(wire.UsageChunk(usage, id, p.RequestedModel, created)); err != nil {
			finish(meter.OutcomeCancelled, err)
			s.logDisconnect(r, p, att)
			return
		}
	}
	_ = sw.Done()

	// Priced from actuals even though the header could not be: the ledger is
	// what the savings report is built from, and it has no such constraint.
	p.Price(&rec, usage)
	finish(meter.OutcomeSuccess, nil)
	s.logCompletion(r, p, rec)
}

// setEstimatedSavings writes the pre-flight figure for a streaming response.
//
// Estimated because output length is unknowable before generation. Flagged as
// such, rather than omitted, because a caller watching a stream still wants a
// signal — and rather than presented as measured, because that would put a
// guess into a field a dashboard might sum.
func (s *Server) setEstimatedSavings(w http.ResponseWriter, p *gateway.Prepared) {
	d := p.Decision
	if !d.SavingMeasured {
		return
	}
	h := w.Header()
	h.Set(HeaderSaved, usd(d.EstimatedSaved))
	h.Set(HeaderCost, usd(d.EstimatedCost))
	h.Set(HeaderBaselineCost, usd(d.BaselineCost))
	h.Set(HeaderSavedEstimated, "true")
	if shadow, ok := d.ShadowSaving(); ok {
		h.Set(HeaderShadowSaved, usd(shadow))
	}
}

func (s *Server) record(rec meter.Record) {
	if s.meter == nil {
		return
	}
	s.meter.Record(rec)
}

func (s *Server) streamsOpened() {
	if s.metrics != nil {
		s.metrics.StreamsActive.Inc()
	}
}

func (s *Server) streamsClosed() {
	if s.metrics != nil {
		s.metrics.StreamsActive.Dec()
	}
}

func outcomeFor(err error) meter.Outcome {
	if err == nil {
		return meter.OutcomeSuccess
	}
	if provider.ClassOf(err) == provider.ClassCancelled {
		// A client hanging up is not a failure of Relay's, and counting it as
		// one would put every cancelled stream into the error budget.
		return meter.OutcomeCancelled
	}
	return meter.OutcomeError
}

// providerMessage extracts a caller-safe description.
//
// provider.Error carries the credential reference and never the secret, so this
// cannot leak one — the guarantee lives in the type, not in this function's
// discipline.
func providerMessage(err error) string {
	var pe *provider.Error
	if errors.As(err, &pe) && pe.Message != "" {
		return pe.Message
	}
	return "upstream provider error"
}

// logCompletion records one finished request.
//
// Token counts and cost; never prompt or response content. Architecture §1
// makes that a product commitment rather than a configuration flag someone
// might forget to set, and the way to keep a commitment like that is to have no
// code path that could violate it.
func (s *Server) logCompletion(r *http.Request, p *gateway.Prepared, rec meter.Record) {
	attrs := []slog.Attr{
		slog.String("request_id", rec.RequestID),
		slog.String("tenant", rec.Tenant),
		slog.String("model_requested", rec.RequestedModel),
		slog.String("endpoint", rec.Endpoint),
		slog.String("route", rec.RouteName),
		slog.String("mode", string(rec.Mode)),
		slog.String("catalog_version", rec.CatalogVersion),
		slog.Int("input_tokens", rec.InputTokens),
		slog.Int("cached_input_tokens", rec.CachedInputTokens),
		slog.Int("output_tokens", rec.OutputTokens),
		slog.Bool("usage_estimated", rec.UsageEstimated),
		slog.String("cost", rec.Cost.String()),
		slog.Bool("substituted", rec.Substituted),
		slog.Duration("duration", rec.Duration),
		slog.Duration("overhead", rec.Overhead()),
	}

	if rec.CacheHit {
		attrs = append(attrs, slog.Bool("cache_hit", true))
	}
	if e := p.Escalation; e != nil {
		// The detail goes here and nowhere else. The header carries the reason
		// code because a header is bounded; this is where "arguments for
		// get_weather do not parse" survives long enough to be useful when
		// somebody asks why a route's escalation rate climbed last Tuesday.
		attrs = append(attrs,
			slog.String("escalated_from", e.From),
			slog.String("escalated_to", e.To),
			slog.String("escalation_reason", string(e.Reason)),
			slog.String("escalation_detail", e.Detail),
			slog.Bool("escalation_recovered", e.Recovered),
			slog.String("discarded_cost", rec.DiscardedCost.String()),
		)
	}
	if len(p.Decision.Optimizations) > 0 {
		// Reasons included here and nowhere else. The header has no room and the
		// ledger keeps only lever names, so this is the record that answers
		// "why is this answer shorter than yesterday's" months later.
		for _, o := range p.Decision.Optimizations {
			attrs = append(attrs, slog.Group("optimization",
				slog.String("lever", o.Lever),
				slog.String("before", o.Before),
				slog.String("after", o.After),
				slog.String("reason", o.Reason),
			))
		}
		attrs = append(attrs, slog.Int("cache_breakpoints", rec.Breakpoints))
	}
	if p.Optimize.Degraded() {
		// Fail-open is silent by construction. Saying so at warn level is the
		// difference between "we stopped optimizing" and "nobody noticed".
		s.log.LogAttrs(r.Context(), slog.LevelWarn, "optimizer degraded",
			slog.String("request_id", rec.RequestID),
			slog.String("outcome", string(p.Optimize.Outcome)),
			slog.Duration("elapsed", p.Optimize.Elapsed),
		)
	}
	if rec.SavingMeasured {
		attrs = append(attrs,
			slog.String("baseline_cost", rec.BaselineCost.String()),
			slog.String("saved", rec.Saved.String()),
		)
		if rec.Counterfactual != "" {
			attrs = append(attrs,
				slog.String("counterfactual", rec.Counterfactual),
				slog.String("shadow_saved", rec.ShadowSaved.String()),
			)
		}
	} else {
		// Unmeasured is not zero. Recording it as zero would let it be summed
		// with real measurements and quietly dilute every savings report that
		// contained one.
		attrs = append(attrs, slog.Bool("saving_measured", false))
	}

	s.log.LogAttrs(r.Context(), slog.LevelInfo, "completion", attrs...)
}

func (s *Server) logAttempt(r *http.Request, p *gateway.Prepared, att execute.Attempt, err error) {
	if err == nil {
		err = att.Err
	}
	class := att.Class
	if err != nil {
		class = provider.ClassOf(err)
	}

	level := slog.LevelWarn
	if class == provider.ClassCancelled {
		level = slog.LevelInfo
	}

	s.log.LogAttrs(r.Context(), level, "attempt failed",
		slog.String("request_id", p.Request.ID),
		slog.String("tenant", p.TenantID()),
		slog.String("endpoint", att.EndpointID),
		slog.String("error_class", string(class)),
		slog.String("error", errString(err)),
	)
}

func (s *Server) logDisconnect(r *http.Request, p *gateway.Prepared, att execute.Attempt) {
	s.log.LogAttrs(r.Context(), slog.LevelInfo, "client disconnected mid-stream",
		slog.String("request_id", p.Request.ID),
		slog.String("tenant", p.TenantID()),
		slog.String("endpoint", att.EndpointID),
	)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
