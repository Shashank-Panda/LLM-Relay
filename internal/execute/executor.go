// Package execute performs the provider call a Decision selected.
//
// Phase 1 makes exactly one attempt. That is a deliberate stopping point rather
// than an unfinished one: retries, failover, and the pre-first-byte boundary
// are Phase 4, and building them before there is an error taxonomy to drive
// them produces retry logic that retries the wrong things.
//
// What is here now is the seam. Every attempt already resolves a credential,
// selects an adapter, and classifies its failure, so Phase 4 adds a loop around
// this file rather than rewriting the call sites.
package execute

import (
	"context"
	"fmt"

	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/provider"
)

// Executor turns a decision into a provider call.
type Executor struct {
	registry *provider.Registry
	resolver provider.Resolver
}

func New(registry *provider.Registry, resolver provider.Resolver) *Executor {
	return &Executor{registry: registry, resolver: resolver}
}

// Attempt records what happened, for logging and later for the usage ledger.
type Attempt struct {
	EndpointID string
	Provider   domain.ProviderID
	Class      provider.ErrorClass
	Err        error
}

// prepare resolves the endpoint, adapter, and credential for a decision.
//
// Failures here are Relay's own, not the provider's, and they are classified
// Terminal: a missing adapter or an unconfigured credential will fail exactly
// the same way on every retry, so spending attempts on them wastes the request
// deadline before failing anyway.
func (e *Executor) prepare(cat *domain.Catalog, endpointID string) (*domain.ModelEndpoint, provider.Adapter, provider.Credential, error) {
	ep, ok := cat.Endpoint(endpointID)
	if !ok {
		return nil, nil, provider.Credential{}, &provider.Error{
			Endpoint: endpointID,
			Class:    provider.ClassTerminal,
			Message:  fmt.Sprintf("endpoint %q is not in catalog %s", endpointID, cat.Version),
		}
	}

	adapter, err := e.registry.For(ep.Provider)
	if err != nil {
		return nil, nil, provider.Credential{}, &provider.Error{
			Provider: string(ep.Provider), Endpoint: ep.ID,
			Class: provider.ClassTerminal, Message: err.Error(),
		}
	}

	cred, err := e.resolver.Resolve(ep.CredentialRef)
	if err != nil {
		// The resolver's message names the missing configuration, never the
		// secret. That is a property of provider.ErrNoCredential, not of care
		// taken here.
		return nil, nil, provider.Credential{}, &provider.Error{
			Provider: string(ep.Provider), Endpoint: ep.ID,
			Class: provider.ClassTerminal, Message: err.Error(),
		}
	}

	return ep, adapter, cred, nil
}

// Chat performs a non-streaming completion against the chosen endpoint.
func (e *Executor) Chat(
	ctx context.Context,
	req *domain.NormalizedRequest,
	cat *domain.Catalog,
	d *domain.Decision,
) (*provider.Response, *domain.ModelEndpoint, Attempt, error) {
	ep, adapter, cred, err := e.prepare(cat, d.Chosen)
	if err != nil {
		return nil, nil, Attempt{EndpointID: d.Chosen, Class: provider.ClassOf(err), Err: err}, err
	}

	att := Attempt{EndpointID: ep.ID, Provider: ep.Provider}

	resp, err := adapter.Chat(ctx, req, ep, cred)
	if err != nil {
		att.Class = provider.ClassOf(err)
		att.Err = err
		return nil, ep, att, err
	}
	return resp, ep, att, nil
}

// Stream begins a streaming completion.
//
// The returned Stream owns a live connection and must be closed by the caller
// on every path, including panic recovery. Nothing here spawns a goroutine: a
// stream reader that outlives its request is the classic way a Go gateway dies
// slowly, and the only reliable defence is not to create one.
func (e *Executor) Stream(
	ctx context.Context,
	req *domain.NormalizedRequest,
	cat *domain.Catalog,
	d *domain.Decision,
) (provider.Stream, *domain.ModelEndpoint, Attempt, error) {
	ep, adapter, cred, err := e.prepare(cat, d.Chosen)
	if err != nil {
		return nil, nil, Attempt{EndpointID: d.Chosen, Class: provider.ClassOf(err), Err: err}, err
	}

	att := Attempt{EndpointID: ep.ID, Provider: ep.Provider}

	st, err := adapter.ChatStream(ctx, req, ep, cred)
	if err != nil {
		att.Class = provider.ClassOf(err)
		att.Err = err
		return nil, ep, att, err
	}
	return st, ep, att, nil
}
