package main

import (
	"fmt"
	"os"
	"time"

	"github.com/Shashank-Panda/relay/internal/catalog"
	"github.com/Shashank-Panda/relay/internal/tenant"
)

// validate checks the configuration a running gateway would load, reports how
// long the price attestations have left, and exits.
//
// It exists because the attestation expiry is a correct check with an
// unacceptable failure mode: a deployment nobody touches eventually refuses to
// start, and the first sign of it is a dead gateway and an error about a date in
// a YAML file. The rule is right — every figure this product reports is
// arithmetic over those prices — so the fix is not to weaken it but to let it be
// asked ahead of time.
//
// Running it with a narrower window than production uses is what turns the
// expiry into a scheduled chore instead of an incident:
//
//	go run ./cmd/relay -validate -max-price-age 1848h   # 76 days = 90 - 14
//
// It also gives CI a way to check that the shipped configuration still loads
// without starting a listener, binding a port, or writing a ledger.
func validate(cfg config) error {
	failed := false

	cat, err := catalog.LoadFile(cfg.catalogPath, catalog.Options{MaxPriceAge: cfg.maxPriceAge})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		failed = true
	} else {
		fmt.Printf("catalog   %s  %d endpoints, %d routes, %d aliases\n",
			cfg.catalogPath, len(cat.Endpoints), len(cat.Routes), len(cat.Aliases))
	}

	// Reported even when the load failed: when it failed *because* of an expired
	// attestation, the table below is the explanation, and printing it is more
	// useful than making the operator re-run with the check disabled.
	ats, ferr := catalog.FreshnessFile(cfg.catalogPath, catalog.Options{
		MaxPriceAge: cfg.maxPriceAge,
	})
	switch {
	case ferr != nil:
		fmt.Fprintln(os.Stderr, ferr)
		failed = true
	case len(ats) == 0:
		fmt.Println("prices    no priced endpoints; nothing to attest")
	default:
		fmt.Printf("prices    %d attested, window %s\n", len(ats), cfg.maxPriceAge)
		for _, a := range ats {
			fmt.Printf("          %-34s verified %s  %s\n",
				a.EndpointID, a.VerifiedOn.Format("2006-01-02"), remaining(a, cfg.maxPriceAge))
		}
		if cfg.maxPriceAge > 0 {
			// The soonest to expire is the one that stops the process, so it is
			// the only one worth a verdict.
			if d := ats[0].Remaining; d <= 0 {
				fmt.Fprintf(os.Stderr,
					"\nprice attestation for %s expired %s ago; re-verify against %s\n",
					ats[0].EndpointID, days(-d), ats[0].Source)
				failed = true
			} else {
				fmt.Printf("\noldest attestation expires in %s (%s)\n",
					days(d), ats[0].EndpointID)
			}
		}
	}

	reg, err := tenant.LoadFile(cfg.tenantsPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		failed = true
	} else {
		ids := reg.IDs()
		fmt.Printf("tenants   %s  %d configured: %v\n", cfg.tenantsPath, len(ids), ids)
	}

	if failed {
		return fmt.Errorf("configuration is not valid")
	}
	fmt.Println("\nOK")
	return nil
}

func remaining(a catalog.Attestation, maxAge time.Duration) string {
	if maxAge <= 0 {
		return "(no expiry check)"
	}
	if a.Remaining <= 0 {
		return "EXPIRED " + days(-a.Remaining) + " ago"
	}
	return days(a.Remaining) + " left"
}

func days(d time.Duration) string {
	n := int(d.Hours() / 24)
	if n == 1 {
		return "1 day"
	}
	return fmt.Sprintf("%d days", n)
}
