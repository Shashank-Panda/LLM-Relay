package server_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Shashank-Panda/relay/internal/catalog"
	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/execute"
	"github.com/Shashank-Panda/relay/internal/gateway"
	"github.com/Shashank-Panda/relay/internal/provider"
	"github.com/Shashank-Panda/relay/internal/provider/openai"
	"github.com/Shashank-Panda/relay/internal/server"
)

// TestReadinessFlipsBeforeTheSocketCloses is the ordering that makes a rolling
// deploy lossless.
//
// Readiness must go false the instant shutdown begins, so load balancers stop
// routing before the listener closes. A process that reports ready while
// draining keeps collecting requests it has already decided not to finish, and
// every one of them becomes a connection reset in somebody's client.
func TestReadinessFlipsBeforeTheSocketCloses(t *testing.T) {
	h := newHarness(t, jsonHandler(upstreamText))

	get := func() (int, string) {
		t.Helper()
		resp, err := http.Get(h.relay.URL + "/readyz")
		if err != nil {
			t.Fatalf("GET /readyz: %v", err)
		}
		defer resp.Body.Close()
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		status, _ := body["status"].(string)
		return resp.StatusCode, status
	}

	if code, status := get(); code != http.StatusOK || status != "ok" {
		t.Fatalf("before shutdown: %d %q, want 200 ok", code, status)
	}

	h.server.BeginShutdown()

	code, status := get()
	if code != http.StatusServiceUnavailable {
		t.Errorf("readyz = %d after BeginShutdown, want 503", code)
	}
	if status != "shutting_down" {
		t.Errorf("status = %q, want shutting_down", status)
	}

	// Liveness stays healthy throughout. Failing it would tell an orchestrator
	// to restart a process that is already exiting cleanly, turning an orderly
	// drain into a kill.
	resp, err := http.Get(h.relay.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz = %d during drain, want 200", resp.StatusCode)
	}
}

// TestInFlightStreamDrains checks the other half: a request already streaming
// when shutdown starts is allowed to finish, rather than being cut off
// mid-response.
//
// Uses a real http.Server rather than httptest, because Shutdown's drain
// behaviour is the thing under test and httptest.Server.Close has different
// semantics — it waits for outstanding requests but is not the code path a
// SIGTERM takes.
func TestInFlightStreamDrains(t *testing.T) {
	t.Setenv("RELAY_CRED_OPENAI_PRIMARY", "sk-test")

	release := make(chan struct{})
	upstream := newSlowUpstream(t, release)

	srv, handler := newRelay(t, upstream.URL)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	httpSrv := &http.Server{Handler: handler}

	served := make(chan error, 1)
	go func() { served <- httpSrv.Serve(ln) }()

	base := "http://" + ln.Addr().String()

	// Start a stream and read its first frame, so the request is genuinely
	// in flight when shutdown begins.
	resp, err := http.Post(base+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"cheap","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 128)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatalf("reading first frame: %v", err)
	}

	srv.BeginShutdown()

	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		shutdownDone <- httpSrv.Shutdown(ctx)
	}()

	// Wait until the listener is genuinely closed before letting the provider
	// finish. Without this the test could release the stream before Shutdown
	// had any effect and pass without exercising the drain at all.
	//
	// A refused new connection alongside a surviving old one is exactly the
	// property under test.
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", ln.Addr().String(), 200*time.Millisecond)
		if err != nil {
			break
		}
		c.Close()
		if time.Now().After(deadline) {
			t.Fatal("the listener never closed; Shutdown is not taking effect")
		}
		time.Sleep(10 * time.Millisecond)
	}

	close(release)

	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("the in-flight stream was cut off during drain: %v", err)
	}
	body := string(buf) + string(rest)

	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("stream did not terminate cleanly during drain:\n%s", body)
	}
	if !strings.Contains(body, "second") {
		t.Errorf("frames written after shutdown began were lost:\n%s", body)
	}

	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown did not return")
	}

	if err := <-served; err != nil && err != http.ErrServerClosed {
		t.Errorf("Serve: %v", err)
	}
}

// newSlowUpstream streams one frame, waits for release, then finishes.
func newSlowUpstream(t *testing.T, release <-chan struct{}) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)

		_, _ = io.WriteString(w, `data: {"id":"c","model":"m","choices":[{"index":0,"delta":{"content":"first"}}]}`+"\n\n")
		fl.Flush()

		select {
		case <-release:
		case <-r.Context().Done():
			return
		case <-time.After(5 * time.Second):
		}

		_, _ = io.WriteString(w, `data: {"id":"c","model":"m","choices":[{"index":0,"delta":{"content":"second"}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"id":"c","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newRelay builds a Server and its handler against an upstream base URL,
// returning both so a test can drive shutdown directly.
func newRelay(t *testing.T, upstreamURL string) (*server.Server, http.Handler) {
	t.Helper()

	cat, err := catalog.Load(strings.NewReader(catalogYAML), catalog.Options{})
	if err != nil {
		t.Fatalf("loading catalog: %v", err)
	}
	for _, ep := range cat.Endpoints {
		ep.BaseURL = upstreamURL
	}

	store := catalog.NewStore(cat)
	client := provider.NewClient(provider.DefaultClientOptions())
	t.Cleanup(client.CloseIdleConnections)

	registry := provider.NewRegistry(openai.New(client))

	gw := &gateway.Gateway{
		Store:    store,
		Executor: execute.New(registry, &provider.EnvResolver{}),
		Policy:   domain.DefaultPolicy(),
	}

	srv := server.New(gw, store, registry, server.Options{
		Logger: slog.New(slog.DiscardHandler),
	})
	return srv, srv.Handler()
}
