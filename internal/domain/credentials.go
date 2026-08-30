package domain

import "sort"

// CredentialSet is which credential refs are usable for one request.
//
// It exists so the router can eliminate an endpoint the caller has no key for
// *before* ranking it, rather than after the executor has spent an attempt
// discovering the same thing. Without it, a caller holding one provider's key
// watches endpoints they cannot reach rank above the ones they can, and then
// pays for that ranking one failed attempt at a time.
//
// It is data, not a callback, and that is deliberate: routing is a pure function
// and must stay one. The set is a snapshot taken outside — exactly as Health is
// — so a future credential store that does real I/O adds latency at one
// identifiable place instead of inside the component whose whole contract is
// that it has none.
//
// A nil set has no opinion and keeps every candidate. That is the ADR-0010
// direction: an absent signal must mean "route normally", never "refuse to
// route". It is also what makes this type additive — every existing caller that
// does not set one behaves exactly as it did.
type CredentialSet struct {
	refs map[string]bool
}

// NewCredentialSet returns a set containing exactly these refs.
//
// Note that NewCredentialSet() with no arguments is *not* the same as nil: it
// is an assertion that nothing is available, and it will reject everything. The
// distinction is the point — "I looked and found none" and "I did not look" lead
// to opposite decisions, and conflating them is how a fail-open system fails
// closed.
func NewCredentialSet(refs ...string) *CredentialSet {
	m := make(map[string]bool, len(refs))
	for _, r := range refs {
		if r != "" {
			m[r] = true
		}
	}
	return &CredentialSet{refs: m}
}

// Has reports whether this ref can be resolved. A nil set answers true.
func (c *CredentialSet) Has(ref string) bool {
	if c == nil {
		return true
	}
	return c.refs[ref]
}

// Refs lists the available refs, sorted, for explanations and dry-run output.
// Never the secrets — this type has never held one and must not learn how.
func (c *CredentialSet) Refs() []string {
	if c == nil || len(c.refs) == 0 {
		return nil
	}
	out := make([]string, 0, len(c.refs))
	for r := range c.refs {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// Missing lists which of the given refs are not available, sorted. A nil set
// reports none missing, consistent with Has.
func (c *CredentialSet) Missing(refs []string) []string {
	if c == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, r := range refs {
		if r == "" || seen[r] || c.refs[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}
