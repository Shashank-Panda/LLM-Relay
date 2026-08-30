// Package respcache is the exact-match response cache.
//
// Exact match only. Approximate prompt matching — a semantic cache — returns
// answers to questions that were not asked, the failure is silent, and the
// debugging story is that somebody eventually notices the model saying something
// slightly wrong. That trade is explicitly out of scope for this product.
//
// The correctness constraints here matter more than the hit rate, because the
// failure modes are asymmetric: a miss costs one provider call, and a wrong hit
// is either a privacy breach or a wrong answer. Three rules, enforced by
// construction rather than by review:
//
//   - Never across tenants. Prompts contain private data, and a cross-tenant hit
//     is a data breach with a nice performance graph. The tenant is the first
//     field of the key *and* is re-checked on the entry, so a hash collision
//     cannot cross the boundary either.
//   - Never when the caller asked for variation. A non-zero temperature is a
//     request for a different answer each time; serving one answer repeatedly is
//     not a cheaper version of that, it is a different request.
//   - Key on everything that affects output. Anything omitted from the key is
//     something two different requests may disagree on while sharing an answer.
package respcache

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"strconv"

	"github.com/Shashank-Panda/relay/internal/domain"
)

// keySchema versions the encoding below.
//
// Bumping it invalidates every existing entry, which is the point: if a field is
// added to the key, entries written under the old encoding were keyed on less
// information than the new code assumes, and reusing them would serve an answer
// whose inputs nobody checked. Cheaper to throw a cache away than to reason
// about what a stale key means.
//
// v2 changed what field two *means* — from the tenant to a cache scope, which
// is the tenant plus, under caller-supplied credentials, a principal derived
// from the key material. Redefining a field is a stronger reason to invalidate
// than adding one: entries written under v1 are keyed on an isolation boundary
// that no longer matches the one being enforced.
const keySchema = "relay/respcache/v2"

// Key derives the cache key for a request as it will actually be sent.
//
// Called with the *optimized* request, not the caller's original. The optimizer
// may have lowered max_tokens or pruned context, and both change the output — so
// keying on the original would let two requests that produce different answers
// share one entry. Cache breakpoints are the exception and are deliberately not
// keyed: a marker changes what a request costs, never what it says.
//
// Deliberately excluded: the stream flag, because a stored answer is replayed
// either way; the request ID; and User/Metadata, which are the customer's own
// analytics labels and do not reach the model.
// scope, not tenant: two callers can share a tenant id and not share a
// credential, which is exactly the situation BYOK creates and the situation in
// which a shared entry is a disclosure rather than a hit.
func Key(scope, endpointID string, req *domain.NormalizedRequest) string {
	h := sha256.New()

	field(h, keySchema)
	field(h, scope)
	field(h, endpointID)

	// System, messages, and tools are the prompt. Order is significant in all
	// three and is preserved: reordering tool definitions changes the prompt the
	// model sees, so it must change the key.
	field(h, "system")
	num(h, len(req.System))
	for _, p := range req.System {
		part(h, p)
	}

	field(h, "messages")
	num(h, len(req.Messages))
	for _, m := range req.Messages {
		field(h, string(m.Role))
		field(h, m.Name)
		num(h, len(m.Parts))
		for _, p := range m.Parts {
			part(h, p)
		}
	}

	field(h, "tools")
	num(h, len(req.Tools))
	for _, t := range req.Tools {
		field(h, t.Name)
		field(h, t.Description)
		field(h, t.Schema)
	}

	field(h, "tool_choice")
	field(h, req.ToolChoice)

	field(h, "response_format")
	field(h, string(req.ResponseFormat.Type))
	field(h, req.ResponseFormat.Schema)

	// Sampling parameters, each with its presence recorded separately from its
	// value. "unset" and "0.0" are different requests — a provider's default
	// temperature is not zero — and an encoding that could not tell them apart
	// would let one answer serve both.
	p := req.Params
	optFloat(h, "temperature", p.Temperature)
	optFloat(h, "top_p", p.TopP)
	optInt(h, "max_tokens", p.MaxTokens)
	optInt64(h, "seed", p.Seed)

	field(h, "effort")
	if p.ReasoningEffort != nil {
		field(h, string(*p.ReasoningEffort))
	} else {
		absent(h)
	}

	field(h, "stop")
	num(h, len(p.Stop))
	for _, s := range p.Stop {
		field(h, s)
	}

	return hex.EncodeToString(h.Sum(nil))
}

// field writes a length-prefixed string.
//
// The length prefix is what makes the encoding injective. Without it the
// concatenation of ("ab", "c") and ("a", "bc") hash identically, and two
// different prompts would share a cache entry — the exact failure an exact-match
// cache exists to be incapable of.
func field(h hash.Hash, s string) {
	num(h, len(s))
	_, _ = h.Write([]byte(s))
}

func num(h hash.Hash, n int) {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(n))
	_, _ = h.Write(buf[:])
}

// absent is a distinct marker for an unset optional value, so that an absent
// field cannot collide with a present empty one.
func absent(h hash.Hash) { _, _ = h.Write([]byte{0xff}) }

func part(h hash.Hash, p domain.ContentPart) {
	field(h, string(p.Kind))
	field(h, p.Text)
	field(h, p.MediaType)
	field(h, p.URL)
	field(h, p.Data)
	field(h, p.ToolCallID)
	field(h, p.ToolName)
	field(h, p.Arguments)
	// CacheBreakpoint is not written. It changes what the request costs, not
	// what it asks, so two requests differing only in marker placement must
	// share an entry rather than miss.
}

func optFloat(h hash.Hash, name string, v *float64) {
	field(h, name)
	if v == nil {
		absent(h)
		return
	}
	// Formatted with full precision rather than written as raw bits, because
	// -0.0 and +0.0 have different bit patterns and are the same request.
	field(h, strconv.FormatFloat(*v, 'g', -1, 64))
}

func optInt(h hash.Hash, name string, v *int) {
	field(h, name)
	if v == nil {
		absent(h)
		return
	}
	num(h, *v)
}

func optInt64(h hash.Hash, name string, v *int64) {
	field(h, name)
	if v == nil {
		absent(h)
		return
	}
	num(h, int(*v))
}
