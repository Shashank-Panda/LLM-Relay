// Package gateway is the request pipeline: resolve the baseline, route,
// execute, and report what happened.
//
// It holds no HTTP types. The server package turns bytes into a
// NormalizedRequest and a Result back into bytes; everything between is here,
// which is what makes the pipeline testable without a socket.
package gateway

import (
	"fmt"

	"github.com/Shashank-Panda/relay/internal/domain"
)

// Baseline sources, recorded on every decision so the savings ledger can say
// where its counterfactual came from.
const (
	SourceExplicitModel = "explicit_model"
	SourceRouteDefault  = "route_default"
	SourceTenantDefault = "tenant_default"
)

// ErrUnknownModel is returned when the model field names nothing the catalog
// knows. A caller error, reported as 400 with the name they sent.
type ErrUnknownModel struct {
	Model          string
	CatalogVersion string
}

func (e *ErrUnknownModel) Error() string {
	return fmt.Sprintf("model %q is not available (catalog %s); "+
		"call GET /v1/models for the list", e.Model, e.CatalogVersion)
}

// ErrRouteHasNoBaseline is returned when a route names no baseline endpoint and
// nothing else can serve the request.
type ErrRouteHasNoBaseline struct{ Route string }

func (e *ErrRouteHasNoBaseline) Error() string {
	return fmt.Sprintf("route %q declares no baseline and no candidate could be selected", e.Route)
}

// ResolveBaseline decides what the caller would have got without Relay.
//
// This runs on every request, in every phase, including Phase 1 where nothing
// is ever substituted. That is the point: the baseline is what makes savings
// measurable, and it can only be established at request time, when the
// information exists. Reconstructing it later from logs is guesswork, and the
// product's central claim would rest on it.
//
// Precedence:
//
//  1. The model names a real endpoint (directly or through an alias) → that
//     endpoint is the baseline, source explicit_model.
//  2. The model names a route → the route's declared baseline, source
//     route_default. A route with none reports savings as unmeasured, which is
//     a different fact from a measured saving of zero.
//  3. Neither → the request fails. Relay does not guess which model a caller
//     meant.
func ResolveBaseline(cat *domain.Catalog, model string, mode domain.BaselineMode) (routeName string, b domain.Baseline, err error) {
	if cat == nil {
		return "", domain.Baseline{}, &ErrUnknownModel{Model: model}
	}

	if ep, ok := cat.Resolve(model); ok {
		// A pinned real model still needs a route, because a route is what
		// names a candidate set and the weights to rank it by. The endpoint's
		// own route is looked up if one exists; otherwise the decision is
		// simply "serve what was asked", which is what strict mode does anyway.
		return routeFor(cat, ep.ID), domain.Baseline{
			EndpointID: ep.ID,
			Mode:       mode,
			Source:     SourceExplicitModel,
		}, nil
	}

	if rt, ok := cat.Route(model); ok {
		return rt.Name, domain.Baseline{
			EndpointID: rt.BaselineID,
			Mode:       mode,
			Source:     SourceRouteDefault,
		}, nil
	}

	return "", domain.Baseline{}, &ErrUnknownModel{Model: model, CatalogVersion: cat.Version}
}

// routeFor finds a route that lists this endpoint as its baseline.
//
// Deterministic by sorted name, because a catalog may legitimately have several
// and the choice must not vary between two identical requests. When none does,
// the empty route name makes the router take its unknown-route path, which
// serves the baseline — exactly the right outcome for a caller who pinned a
// specific model.
func routeFor(cat *domain.Catalog, endpointID string) string {
	for _, name := range domain.SortedKeys(cat.Routes) {
		if cat.Routes[name].BaselineID == endpointID {
			return name
		}
	}
	return ""
}

// ResolveMode reads the request's pin header against the tenant policy.
//
// An explicit pin always wins. X-Relay-Pin: strict is the escape hatch a
// customer needs on the one request where substitution is unacceptable, and an
// escape hatch a policy can override is not one.
func ResolveMode(pin string, pol *domain.Policy) domain.BaselineMode {
	switch domain.BaselineMode(pin) {
	case domain.ModeStrict:
		return domain.ModeStrict
	case domain.ModeShadow:
		return domain.ModeShadow
	case domain.ModeOptimize:
		// Requesting optimization does not grant it. The tenant policy still
		// decides, because permission to substitute is theirs to give and a
		// per-request header is not where that consent lives.
		return pol.Mode("")
	}
	return pol.Mode("")
}
