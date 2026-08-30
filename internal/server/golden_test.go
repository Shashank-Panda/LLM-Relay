package server_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Shashank-Panda/relay/internal/catalog"
	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/execute"
	"github.com/Shashank-Panda/relay/internal/gateway"
	"github.com/Shashank-Panda/relay/internal/provider"
	"github.com/Shashank-Panda/relay/internal/provider/anthropic"
	"github.com/Shashank-Panda/relay/internal/provider/ollama"
	"github.com/Shashank-Panda/relay/internal/provider/openai"
	"github.com/Shashank-Panda/relay/internal/server"
	"github.com/Shashank-Panda/relay/internal/tenant"
)

var updateGolden = flag.Bool("update", false, "rewrite the console's dry-run fixture")

const goldenPath = "../../web/fixtures/dryrun.golden.json"

// TestDryRunGoldenFixture pins the dry-run payload across a language boundary.
//
// The console is a separate application in another language that renders this
// document, so its shape is now a published interface with a second consumer.
// Without something holding it still, a renamed Go struct tag is discovered by
// somebody looking at `undefined` in a browser, days later, with nothing
// pointing at the commit that caused it.
//
// So the Go side owns the file and the console reads it: `go test -run
// GoldenFixture -update ./internal/server` regenerates, and `web/lib/dryrun.test.ts`
// parses the result with the schemas the console actually uses. A field that
// changes therefore breaks a Go test first — the earliest place it can be
// noticed and the cheapest place to fix it.
//
// This extends a habit the project already has. TestWorkedExample_MatchesDocumentation
// reproduces routing.md §7 to the published decimal so the prose cannot drift
// from the code; this is the same argument applied to a consumer that happens
// not to be written in Go.
func TestDryRunGoldenFixture(t *testing.T) {
	relay := newGoldenHarness(t)

	req, err := http.NewRequest(http.MethodPost, relay.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"relay/fast-coder","messages":[{"role":"user","content":"write a bash one-liner"}]}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-relay-demo-optimize-0f1e2d3c4b5a")
	req.Header.Set(server.HeaderDryRun, "1")
	// The fixture is generated the way the Explain tab actually asks, so what
	// the console is tested against is what it will receive.
	req.Header.Set(server.HeaderAssumeCredentials, "all")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var pretty bytes.Buffer
	if err := json.Indent(&pretty, raw, "", "  "); err != nil {
		t.Fatalf("indent: %v", err)
	}
	pretty.WriteByte('\n')

	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		var doc any
		if err := json.Unmarshal(pretty.Bytes(), &doc); err != nil {
			t.Fatalf("unmarshal for normalize: %v", err)
		}
		normalizeGolden(doc)
		out, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		out = append(out, byte('\n'))
		if err := os.WriteFile(goldenPath, out, 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		t.Logf("wrote %s", goldenPath)
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read fixture (regenerate with -update): %v", err)
	}

	// Compared as parsed documents rather than as bytes: the console cares
	// about the shape and the values, and failing on key order or indentation
	// would make this a formatting test that cries wolf.
	var got, expected any
	if err := json.Unmarshal(pretty.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal got: %v", err)
	}
	if err := json.Unmarshal(want, &expected); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}

	// A fixture that silently became empty would pass every comparison below.
	if m, ok := expected.(map[string]any); !ok || m["ranked"] == nil || m["credentials"] == nil {
		t.Fatalf("the fixture is missing the fields the console renders; regenerate it")
	}

	normalizeGolden(got)
	normalizeGolden(expected)

	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(expected)
	if !bytes.Equal(gotJSON, wantJSON) {
		t.Errorf("the dry-run payload changed.\n\ngot:\n%s\n\nfixture:\n%s\n\n"+
			"If this change is intended, regenerate with:\n"+
			"  go test ./internal/server -run GoldenFixture -update\n"+
			"and update web/lib/types.ts to match, because the console parses this.",
			pretty.String(), want)
	}
}

// normalizeGolden neutralises the one field in the payload that is a
// measurement rather than a decision.
//
// optimizer.elapsed_us is how long the optimizer actually ran, so it differs
// between two runs on the same machine and between machines. That made this
// test pass on Windows and fail on Linux for a while, and the reason is worth
// recording: Windows' clock is coarse enough that a few microseconds round to
// 0 every time, so the fixture looked stable when it was only ever stable by
// accident. On Linux it is a small number that varies per run, which would
// have flaked in CI rather than failing honestly.
//
// The budget beside it is configuration, not a measurement, and stays pinned —
// a change to the optimizer's time budget is exactly the kind of thing this
// fixture should catch. Only the stopwatch is dropped.
func normalizeGolden(doc any) {
	m, ok := doc.(map[string]any)
	if !ok {
		return
	}
	opt, ok := m["optimizer"].(map[string]any)
	if !ok {
		return
	}
	if _, present := opt["elapsed_us"]; present {
		opt["elapsed_us"] = float64(0)
	}
}

// newGoldenHarness serves the *shipped* configuration.
//
// The shipped files, not a fixture catalog: the console is pointed at a real
// deployment, so what it is tested against should be what a real deployment
// produces. No upstream is needed at all — a dry run is answered before any
// provider is contacted, which is the property that makes the Explain tab free.
func newGoldenHarness(t *testing.T) *httptest.Server {
	t.Helper()

	// No credentials configured, deliberately: the fixture must be the one a
	// reader with no keys receives, since that is the audience the Explain tab
	// exists for.
	cat, err := catalog.LoadFile(filepath.Join("..", "..", "config", "catalog.yaml"),
		catalog.Options{MaxPriceAge: 0})
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	store := catalog.NewStore(cat)

	tenants, err := tenant.LoadFile(filepath.Join("..", "..", "config", "tenants.yaml"))
	if err != nil {
		t.Fatalf("tenants: %v", err)
	}

	client := provider.NewClient(provider.DefaultClientOptions())
	registry := provider.NewRegistry(ollama.New(client), openai.New(client), anthropic.New(client))
	resolver := &provider.EnvResolver{Free: map[string]bool{"local": true}}

	srv := server.New(&gateway.Gateway{
		Store:       store,
		Executor:    execute.New(registry, resolver),
		Credentials: resolver,
		Tenants:     tenants,
		Policy:      domain.DefaultPolicy(),
	}, store, registry, server.Options{Logger: slog.New(slog.DiscardHandler), Tenants: tenants})

	relay := httptest.NewServer(srv.Handler())
	t.Cleanup(relay.Close)
	return relay
}
