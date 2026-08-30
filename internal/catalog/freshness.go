package catalog

import (
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

// Attestation is one priced endpoint's pricing.verified_on and what is left of
// its life.
type Attestation struct {
	EndpointID string
	Source     string
	VerifiedOn time.Time

	// Expires is when the loader will begin refusing this entry, and Remaining
	// is how long until then. Remaining is negative once it has passed.
	Expires   time.Time
	Remaining time.Duration
}

// Freshness reports every priced endpoint's attestation, soonest to expire
// first.
//
// It exists because the expiry is a scheduled, silent, total failure. The
// loader is right to refuse prices nobody has confirmed — every figure this
// product reports is arithmetic over them — but the consequence is that a
// deployment left alone eventually stops starting, with an error about a date
// in a YAML file and no prior warning. That is the worst possible way for a
// correct check to behave.
//
// So the check gets a second face: the same rule, asked ahead of time and
// answered as a number of days rather than as a refusal to boot. CI runs it
// against a narrower window than production uses, which turns a calendar event
// into a red build a fortnight early.
//
// Deliberately re-parses rather than reading domain.Catalog: verified_on is
// dropped during conversion, correctly, because routing has no use for it. This
// is the one caller that does, and giving the domain type a field for it would
// put a compliance date on the hot path to serve a command-line flag.
func Freshness(r io.Reader, opts Options) ([]Attestation, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)

	var f file
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("catalog: parse: %w", err)
	}

	now := opts.now()
	maxAge := opts.MaxPriceAge

	var out []Attestation
	for _, e := range f.Endpoints {
		// A free endpoint attests to nothing, and has nothing to expire. A
		// local model is the case, and reporting it as "no attestation" would
		// put a permanent warning on a correct configuration.
		if e.Pricing.isFree() || e.Pricing.VerifiedOn == "" {
			continue
		}
		verified, err := time.Parse(verifiedOnLayout, e.Pricing.VerifiedOn)
		if err != nil {
			return nil, fmt.Errorf("catalog: endpoint %s: pricing.verified_on %q is not a YYYY-MM-DD date",
				e.ID, e.Pricing.VerifiedOn)
		}

		a := Attestation{
			EndpointID: e.ID,
			Source:     sourceOrProviderPage(e.Pricing.Source),
			VerifiedOn: verified,
		}
		if maxAge > 0 {
			a.Expires = verified.Add(maxAge)
			a.Remaining = a.Expires.Sub(now)
		}
		out = append(out, a)
	}

	// Soonest to expire first: the only entry that matters is the one that will
	// stop the process, and it should not have to be searched for.
	sort.Slice(out, func(i, j int) bool {
		if out[i].VerifiedOn.Equal(out[j].VerifiedOn) {
			return out[i].EndpointID < out[j].EndpointID
		}
		return out[i].VerifiedOn.Before(out[j].VerifiedOn)
	})
	return out, nil
}

// FreshnessFile is Freshness against a path.
func FreshnessFile(path string, opts Options) ([]Attestation, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	defer f.Close()
	return Freshness(f, opts)
}
