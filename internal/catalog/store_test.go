package catalog

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Shashank-Panda/relay/internal/domain"
)

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestStoreSwap(t *testing.T) {
	t.Run("empty store", func(t *testing.T) {
		if got := NewStore(nil).Current(); got != nil {
			t.Errorf("Current() = %v, want nil", got)
		}
	})

	t.Run("swap returns the previous snapshot", func(t *testing.T) {
		first := mustLoad(t, minimal)
		s := NewStore(first)

		second := mustLoad(t, minimal)
		if prev := s.Swap(second); prev != first {
			t.Error("Swap did not return the previous snapshot")
		}
		if s.Current() != second {
			t.Error("Current() did not return the new snapshot")
		}
	})

	t.Run("swapping nil is a no-op", func(t *testing.T) {
		// Guards against a reload path that computes a nil catalog and blanks
		// out a working one.
		first := mustLoad(t, minimal)
		s := NewStore(first)

		s.Swap(nil)

		if s.Current() != first {
			t.Error("a nil swap replaced the live snapshot")
		}
	})
}

func TestStoreReload(t *testing.T) {
	t.Run("a valid edit is installed", func(t *testing.T) {
		path := writeTemp(t, "catalog.yaml", minimal)
		s := NewStore(nil)

		if err := s.ReloadFile(path, testOptions()); err != nil {
			t.Fatalf("ReloadFile: %v", err)
		}
		if s.Current() == nil {
			t.Fatal("Current() is nil after a successful reload")
		}
		if _, ok := s.Current().Endpoint("p/a@r"); !ok {
			t.Error("reloaded catalog is missing its endpoint")
		}
	})

	// The property this whole ordering exists for: an operator's typo must
	// degrade to "running yesterday's prices", never to "not running".
	// See ADR-0010.
	t.Run("a broken edit leaves the previous catalog serving", func(t *testing.T) {
		path := writeTemp(t, "catalog.yaml", minimal)
		s := NewStore(nil)
		if err := s.ReloadFile(path, testOptions()); err != nil {
			t.Fatalf("initial load: %v", err)
		}
		good := s.Current()

		for _, broken := range []struct{ name, content string }{
			{"malformed yaml", "version: \"v2\"\nendpoints: [oh dear"},
			{"typo'd field", "version: \"v2\"\nendpoints:\n  - identifier: p/b@r\n"},
			{"empty file", ""},
			{"semantically invalid", `
version: "v2"
endpoints:
  - id: p/b@r
    provider: p
    model: b
    limits: {context_window: 100000}
    pricing: {input: 1.00, output: 2.00}
`},
		} {
			t.Run(broken.name, func(t *testing.T) {
				if err := os.WriteFile(path, []byte(broken.content), 0o600); err != nil {
					t.Fatalf("write: %v", err)
				}

				if err := s.ReloadFile(path, testOptions()); err == nil {
					t.Fatal("a broken catalog reloaded successfully")
				}
				if s.Current() != good {
					t.Error("a failed reload disturbed the live snapshot")
				}
			})
		}
	})

	t.Run("a missing file leaves the previous catalog serving", func(t *testing.T) {
		s := NewStore(mustLoad(t, minimal))
		good := s.Current()

		if err := s.ReloadFile(filepath.Join(t.TempDir(), "gone.yaml"), testOptions()); err == nil {
			t.Fatal("loading a missing file succeeded")
		}
		if s.Current() != good {
			t.Error("a failed reload disturbed the live snapshot")
		}
	})
}

// TestStoreConcurrentReadsDuringSwap asserts that readers always observe a
// complete, self-consistent snapshot — never a catalog whose endpoints have
// been replaced but whose routes have not.
func TestStoreConcurrentReadsDuringSwap(t *testing.T) {
	s := NewStore(mustLoad(t, minimal))

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				cat := s.Current()
				if cat == nil {
					t.Error("observed a nil catalog")
					return
				}
				// Every route's candidates must resolve within the same
				// snapshot. A torn read would surface here.
				for _, name := range domain.SortedKeys(cat.Routes) {
					for _, id := range cat.Routes[name].Candidates {
						if _, ok := cat.Endpoint(id); !ok {
							t.Errorf("route %s references missing endpoint %s", name, id)
							return
						}
					}
				}
			}
		}()
	}

	for range 200 {
		s.Swap(mustLoad(t, minimal))
	}
	close(stop)
	wg.Wait()
}
