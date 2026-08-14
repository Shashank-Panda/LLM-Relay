package execute

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/provider"
)

// The failures here are Relay's own, not a provider's, and every one is
// classified Terminal. That is the load-bearing property: a missing adapter or
// an unconfigured credential fails identically on every retry, so once Phase 4
// adds a retry loop, spending attempts on these would burn the request deadline
// before failing anyway.

type stubAdapter struct {
	id   domain.ProviderID
	resp *provider.Response
	err  error

	// gotCred records what the executor handed over, so the credential seam can
	// be asserted without the adapter having to leak it anywhere else.
	gotCred provider.Credential
}

func (s *stubAdapter) ID() domain.ProviderID { return s.id }

func (s *stubAdapter) Chat(_ context.Context, _ *domain.NormalizedRequest, _ *domain.ModelEndpoint, c provider.Credential) (*provider.Response, error) {
	s.gotCred = c
	return s.resp, s.err
}

func (s *stubAdapter) ChatStream(_ context.Context, _ *domain.NormalizedRequest, _ *domain.ModelEndpoint, c provider.Credential) (provider.Stream, error) {
	s.gotCred = c
	return nil, s.err
}

func (s *stubAdapter) ClassifyError(r *http.Response, err error) provider.ErrorClass {
	return provider.DefaultClassify(r, err)
}

func catalogWith(ep *domain.ModelEndpoint) *domain.Catalog {
	return &domain.Catalog{
		Version:   "v1",
		Endpoints: map[string]*domain.ModelEndpoint{ep.ID: ep},
	}
}

func endpoint() *domain.ModelEndpoint {
	return &domain.ModelEndpoint{
		ID: "openai/x@us", Provider: "openai", Model: "x",
		Deployment: "us", CredentialRef: "openai-primary",
	}
}

func decisionFor(id string) *domain.Decision { return &domain.Decision{Chosen: id} }

func TestPrepareFailures(t *testing.T) {
	tests := []struct {
		name     string
		catalog  *domain.Catalog
		registry *provider.Registry
		resolver provider.Resolver
		chosen   string
		wantIn   string
	}{
		{
			// Routing chose something the snapshot does not contain, which can
			// only happen if the two disagreed — worth naming the catalog
			// version so the disagreement is findable.
			name:     "endpoint absent from the catalog",
			catalog:  catalogWith(endpoint()),
			registry: provider.NewRegistry(&stubAdapter{id: "openai"}),
			resolver: provider.StaticResolver{"openai-primary": {Ref: "openai-primary"}},
			chosen:   "openai/ghost@us",
			wantIn:   "not in catalog v1",
		},
		{
			// A latent outage sitting in a config file: routing can select this
			// endpoint and execution cannot reach it.
			name:     "no adapter for the provider",
			catalog:  catalogWith(endpoint()),
			registry: provider.NewRegistry(&stubAdapter{id: "cohere"}),
			resolver: provider.StaticResolver{"openai-primary": {Ref: "openai-primary"}},
			chosen:   "openai/x@us",
			wantIn:   "no adapter registered",
		},
		{
			// A deployment mistake, not a bad key. Saying so keeps the operator
			// out of a provider dashboard chasing a 401 that never happened.
			name:     "credential not configured",
			catalog:  catalogWith(endpoint()),
			registry: provider.NewRegistry(&stubAdapter{id: "openai"}),
			resolver: provider.StaticResolver{},
			chosen:   "openai/x@us",
			wantIn:   "no credential configured",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := New(tc.registry, tc.resolver)

			// Both call shapes must fail identically. A streaming request that
			// took a different path on a configuration error would report a
			// different status than the non-streaming one for the same fault.
			for _, call := range []struct {
				kind string
				run  func() (Attempt, error)
			}{
				{"Chat", func() (Attempt, error) {
					_, _, att, err := e.Chat(t.Context(), &domain.NormalizedRequest{}, tc.catalog, decisionFor(tc.chosen))
					return att, err
				}},
				{"Stream", func() (Attempt, error) {
					_, _, att, err := e.Stream(t.Context(), &domain.NormalizedRequest{}, tc.catalog, decisionFor(tc.chosen))
					return att, err
				}},
			} {
				t.Run(call.kind, func(t *testing.T) {
					att, err := call.run()
					if err == nil {
						t.Fatal("no error")
					}
					if !strings.Contains(err.Error(), tc.wantIn) {
						t.Errorf("err = %q, want it to mention %q", err, tc.wantIn)
					}
					if got := provider.ClassOf(err); got != provider.ClassTerminal {
						t.Errorf("class = %s, want Terminal — retrying this cannot help", got)
					}
					if att.Class != provider.ClassTerminal {
						t.Errorf("attempt class = %s, want Terminal", att.Class)
					}
					// The endpoint is recorded even on failure, or the log line
					// cannot say which endpoint failed.
					if att.EndpointID != tc.chosen {
						t.Errorf("attempt endpoint = %q, want %q", att.EndpointID, tc.chosen)
					}
					// The secret must not travel in an error, which will be
					// logged.
					if strings.Contains(err.Error(), "sk-") {
						t.Errorf("error leaked a credential: %s", err)
					}
				})
			}
		})
	}
}

func TestChatPassesTheResolvedCredential(t *testing.T) {
	stub := &stubAdapter{id: "openai", resp: &provider.Response{ID: "r"}}
	e := New(
		provider.NewRegistry(stub),
		provider.StaticResolver{"openai-primary": {Ref: "openai-primary", APIKey: "sk-real"}},
	)

	resp, ep, att, err := e.Chat(t.Context(), &domain.NormalizedRequest{},
		catalogWith(endpoint()), decisionFor("openai/x@us"))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if resp.ID != "r" {
		t.Errorf("response = %+v", resp)
	}
	if ep.ID != "openai/x@us" {
		t.Errorf("endpoint = %q", ep.ID)
	}
	if att.Provider != "openai" || att.Class != "" {
		t.Errorf("attempt = %+v, want no error class on success", att)
	}
	if stub.gotCred.APIKey != "sk-real" {
		t.Errorf("adapter received %q, want the resolved key", stub.gotCred.Ref)
	}
}

// A provider failure keeps its own classification rather than being flattened
// into Terminal. Phase 4's retry loop reads exactly this field.
func TestProviderErrorKeepsItsClass(t *testing.T) {
	upstream := &provider.Error{
		Provider: "openai", Endpoint: "openai/x@us",
		Class: provider.ClassRetrySame, HTTPStatus: 429, Message: "slow down",
	}
	e := New(
		provider.NewRegistry(&stubAdapter{id: "openai", err: upstream}),
		provider.StaticResolver{"openai-primary": {Ref: "openai-primary"}},
	)

	_, _, att, err := e.Chat(t.Context(), &domain.NormalizedRequest{},
		catalogWith(endpoint()), decisionFor("openai/x@us"))

	var pe *provider.Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want *provider.Error", err)
	}
	if att.Class != provider.ClassRetrySame {
		t.Errorf("attempt class = %s, want RetrySame", att.Class)
	}
	if att.Err == nil {
		t.Error("attempt did not record the error")
	}
}
