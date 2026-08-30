package server

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/Shashank-Panda/relay/internal/provider"
)

// parseCredentials reads caller-supplied provider keys off a request.
//
// Format is one header per credential ref, repeatable:
//
//	X-Relay-Credential: openai-primary sk-...
//	X-Relay-Credential: anthropic-primary sk-ant-...
//
// Spelled like Authorization's own "scheme value" shape so there is nothing to
// base64 or JSON-encode wrongly from a shell, and repeatable rather than
// comma-joined because provider keys are opaque strings and a separator that
// could appear inside one is a parser waiting to split a secret in half.
//
// The ref is the same name the catalog uses in credential_ref and the same one
// EnvResolver derives its variable from, so there is a single naming rule in the
// system rather than two that can drift.
//
// Errors never quote the value. An error body is one of the places a credential
// most reliably escapes into a log, a screenshot, or a bug report, so the
// position is named and the content is not.
func parseCredentials(h http.Header) (*provider.KeySet, error) {
	values := h.Values(HeaderCredential)
	if len(values) == 0 {
		return nil, nil
	}

	creds := make(map[string]string, len(values))
	for i, v := range values {
		ref, secret, ok := strings.Cut(strings.TrimSpace(v), " ")
		if !ok {
			return nil, fmt.Errorf("%s[%d] is not %q", HeaderCredential, i, "<ref> <key>")
		}
		ref = strings.TrimSpace(ref)
		secret = strings.TrimSpace(secret)
		if ref == "" || secret == "" {
			return nil, fmt.Errorf("%s[%d] is not %q", HeaderCredential, i, "<ref> <key>")
		}
		if _, dup := creds[ref]; dup {
			// Two values for one ref is ambiguous, and picking either silently
			// means a caller who rotated a key mid-script cannot tell which one
			// was used.
			return nil, fmt.Errorf("%s names %q more than once", HeaderCredential, ref)
		}
		creds[ref] = secret
	}
	return provider.NewKeySet(creds), nil
}

// insecureCredentials reports a credential sent over a plaintext connection.
//
// A key in the clear is a compromised key, and the request that carried it will
// succeed, which means nothing else in the system will ever mention it. Refused
// by default and allowed explicitly, because local development over http is a
// real and common case — the compose stack is one — and the choice belongs to
// whoever deployed this rather than to whoever wrote it.
func insecureCredentials(r *http.Request, allow bool) bool {
	if allow || r.TLS != nil {
		return false
	}
	// A terminating proxy is the normal production shape, so its own statement
	// about the original scheme is trusted here. It is a header the caller can
	// forge — but a caller who forges it is lying about their own connection to
	// weaken their own key, which is not a threat this check exists to stop.
	if strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		return false
	}
	return true
}

// resolverOf converts a possibly-nil *KeySet into a Resolver interface that is
// actually nil when there is no key set.
//
// This exists because Go's typed-nil rule is a trap with real consequences
// here: assigning a nil *KeySet straight into a Resolver produces a non-nil
// interface, so every "did the caller supply credentials?" check downstream
// answers yes. The observed failure was that requests with no credential header
// at all were routed as though the caller held nothing — an empty availability
// set rather than an absent one — which eliminated every candidate and silently
// dropped each request onto its route's fallback.
//
// The distinction between "supplied nothing" and "supplied an empty set" is
// load-bearing throughout this change, and this is the one place the language
// will quietly erase it.
func resolverOf(k *provider.KeySet) provider.Resolver {
	if k == nil {
		return nil
	}
	return k
}
