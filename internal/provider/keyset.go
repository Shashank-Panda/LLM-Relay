package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strconv"
	"strings"

	"github.com/Shashank-Panda/relay/internal/domain"
)

// KeySet is one request's caller-supplied provider credentials, keyed by
// credential ref.
//
// It is itself a Resolver, so bringing your own key and reading one from the
// environment compose instead of branching: Chain{keySet, envResolver} is the
// whole of the BYOK story at the executor, and dropping the second element is
// the whole of the difference between a self-hosted install and a hosted one.
//
// It never outlives the request that carried it. Nothing caches it, nothing
// writes it down, and the two String methods below make sure nothing prints it.
type KeySet struct {
	byRef map[string]Credential
}

// NewKeySet returns a set holding these credentials. Empty refs and empty
// secrets are dropped: a blank key is indistinguishable at the provider from a
// revoked one, and accepting it here would turn a client-side mistake into a
// confusing 401 from someone else's API.
func NewKeySet(creds map[string]string) *KeySet {
	k := &KeySet{byRef: make(map[string]Credential, len(creds))}
	for ref, secret := range creds {
		if ref == "" || secret == "" {
			continue
		}
		k.byRef[ref] = Credential{Ref: ref, APIKey: secret}
	}
	if len(k.byRef) == 0 {
		return nil
	}
	return k
}

// Resolve returns the caller's own credential for this endpoint.
func (k *KeySet) Resolve(_ context.Context, _ string, ep *domain.ModelEndpoint) (Credential, error) {
	ref := ""
	if ep != nil {
		ref = ep.CredentialRef
	}
	if k == nil {
		return Credential{}, &ErrNoCredential{Ref: ref, Hint: keySetHint(ref)}
	}
	c, ok := k.byRef[ref]
	if !ok {
		return Credential{}, &ErrNoCredential{Ref: ref, Hint: keySetHint(ref)}
	}
	return c, nil
}

// Available reports whether the caller supplied this ref.
func (k *KeySet) Available(_, ref string) bool {
	if k == nil {
		return false
	}
	_, ok := k.byRef[ref]
	return ok
}

// Refs lists the refs supplied, sorted. Never the secrets.
func (k *KeySet) Refs() []string {
	if k == nil {
		return nil
	}
	out := make([]string, 0, len(k.byRef))
	for ref := range k.byRef {
		out = append(out, ref)
	}
	sort.Strings(out)
	return out
}

// Principal derives a stable, non-reversible identifier for this exact set of
// credentials, for use as a cache isolation scope.
//
// Two callers presenting different keys must hash differently and the same
// caller must hash identically across requests; beyond that the value carries no
// meaning and is never intended to be interpreted. It is truncated because a
// cache scope needs collision resistance between a handful of concurrent users,
// not the full strength of the digest — and a shorter value is less inviting to
// treat as an identifier for something else.
//
// The digest covers refs and secrets with explicit lengths, so that two
// different sets cannot produce the same preimage by rearranging where one
// field ends and the next begins.
func (k *KeySet) Principal(schema string) string {
	if k == nil || len(k.byRef) == 0 {
		return ""
	}
	h := sha256.New()
	writeLenPrefixed(h, schema)
	for _, ref := range k.Refs() {
		writeLenPrefixed(h, ref)
		writeLenPrefixed(h, k.byRef[ref].APIKey)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// String redacts, for the same reason Credential.String does and with more at
// stake: without this, printing an execute.Request with %v renders a map of
// live provider keys into whatever is reading it.
func (k *KeySet) String() string {
	if k == nil || len(k.byRef) == 0 {
		return "keyset(empty)"
	}
	return "keyset(" + strconv.Itoa(len(k.byRef)) + " refs: " + strings.Join(k.Refs(), ", ") + ")"
}

// GoString redacts under %#v too, which is what most debug printing uses.
func (k *KeySet) GoString() string { return k.String() }

func keySetHint(ref string) string {
	if ref == "" {
		return ""
	}
	return "send X-Relay-Credential: " + ref + " <key>"
}

func writeLenPrefixed(h interface{ Write([]byte) (int, error) }, s string) {
	_, _ = h.Write([]byte(strconv.Itoa(len(s))))
	_, _ = h.Write([]byte{':'})
	_, _ = h.Write([]byte(s))
}

// Chain resolves against each resolver in order, returning the first credential
// found.
//
// The order is the policy: caller-supplied first, deployment environment second.
// That is right for both shapes without a flag — self-hosted, the operator's own
// keys serve everyone and a caller may still override; hosted, the environment
// holds nothing and the caller's key is the only source. "Hosted is a
// configuration change rather than a rewrite" is literally true because of this
// type: the change is dropping the second element.
type Chain []Resolver

func (c Chain) Resolve(ctx context.Context, tenant string, ep *domain.ModelEndpoint) (Credential, error) {
	ref := ""
	if ep != nil {
		ref = ep.CredentialRef
	}

	var hints []string
	for _, r := range c {
		if r == nil {
			continue
		}
		cred, err := r.Resolve(ctx, tenant, ep)
		if err == nil {
			return cred, nil
		}
		// Collect each resolver's own remedy. A hosted caller told only to set
		// an environment variable has been sent to a machine they do not own;
		// a self-hosted operator told only to send a header has been sent past
		// the configuration file they were already editing.
		var missing *ErrNoCredential
		if errors.As(err, &missing) {
			if missing.Hint != "" && !contains(hints, missing.Hint) {
				hints = append(hints, missing.Hint)
			}
			continue
		}
		// Anything that is not "no credential here" is a real failure and is
		// reported rather than swallowed by the next link.
		return Credential{}, err
	}
	return Credential{}, &ErrNoCredential{Ref: ref, Hint: strings.Join(hints, ", or ")}
}

// Available reports whether any link can supply this ref.
func (c Chain) Available(tenant, ref string) bool {
	for _, r := range c {
		if r == nil {
			continue
		}
		if a, ok := r.(Availability); ok && a.Available(tenant, ref) {
			return true
		}
	}
	return false
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
