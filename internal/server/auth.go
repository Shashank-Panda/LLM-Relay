package server

import (
	"context"
	"net/http"

	"github.com/Shashank-Panda/relay/internal/tenant"
	"github.com/Shashank-Panda/relay/internal/wire"
)

const ctxTenant ctxKey = 1

// withTenant resolves the API key to a tenant and attaches it to the context.
//
// An absent key is not an error: it resolves to the default tenant, which is
// exactly Phase 1's behaviour and keeps an existing deployment working
// unchanged. A *wrong* key is a 401 — presenting credentials that do not match
// is a mistake worth reporting, unlike presenting none.
//
// Phase 7 owns real authentication. What this does is give the savings ledger a
// tenant dimension, without which a per-tenant report would have to be
// reconstructed later from data that never carried the field.
func withTenant(reg *tenant.Registry) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if reg == nil {
				next.ServeHTTP(w, r)
				return
			}

			key := tenant.BearerToken(r.Header.Get("Authorization"))
			t, ok := reg.Resolve(key)
			if !ok {
				// Deliberately says nothing about which part was wrong, and
				// never echoes the key — an error body is a place credentials
				// leak into logs and screenshots.
				wire.WriteError(w, http.StatusUnauthorized,
					"invalid API key", wire.TypeAuthentication, "invalid_api_key")
				return
			}

			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxTenant, t)))
		})
	}
}

// tenantOf returns the request's tenant, falling back to the default so that a
// handler never has to nil-check. Metering must never lose a record because
// authentication was not configured.
func tenantOf(ctx context.Context, reg *tenant.Registry) *tenant.Tenant {
	if t, ok := ctx.Value(ctxTenant).(*tenant.Tenant); ok && t != nil {
		return t
	}
	if reg != nil {
		return reg.Default()
	}
	return nil
}
