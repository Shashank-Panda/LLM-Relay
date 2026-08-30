import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

import { candidateRow, percent, rankedRows, usd, verdict } from "./dryrun";
import { DryRun } from "./types";

/**
 * The fixture is written by a Go test against the shipped configuration:
 *
 *   go test ./internal/server -run GoldenFixture -update
 *
 * Reading it here is what closes the loop across the language boundary. A
 * renamed struct tag fails the Go test first; if it somehow does not, it fails
 * the schema parse below with the field name, rather than rendering `undefined`
 * in a browser days later.
 */
const raw = JSON.parse(
  readFileSync(new URL("../fixtures/dryrun.golden.json", import.meta.url), "utf8"),
);

describe("the gateway and the console agree on the dry-run shape", () => {
  it("parses the fixture the gateway actually produces", () => {
    const parsed = DryRun.safeParse(raw);
    if (!parsed.success) {
      throw new Error(
        `schema drift: ${parsed.error.issues
          .map((i) => `${i.path.join(".")}: ${i.message}`)
          .join("; ")}`,
      );
    }
    expect(parsed.data.object).toBe("relay.dry_run");
  });
});

const doc = DryRun.parse(raw);

describe("the ranked table", () => {
  it("has components that sum to each total", () => {
    // This is the claim the stacked bar makes visually, and it is the whole
    // reason the components are exposed at all: the ranking answers "why not
    // the better model" with arithmetic rather than an assurance. If the parts
    // stop adding up, the chart is a lie that looks fine.
    const rows = rankedRows(doc, () => undefined);
    expect(rows.length).toBeGreaterThan(0);
    for (const row of rows) {
      expect(row.inconsistent, `${row.endpoint} components do not sum to its total`).toBe(false);
    }
  });

  it("orders segments largest first, so the dominant dimension reads first", () => {
    for (const row of rankedRows(doc, () => undefined)) {
      const values = row.segments.map((s) => s.value);
      expect(values).toEqual([...values].sort((a, b) => b - a));
    }
  });

  it("flags a candidate whose components contradict its total", () => {
    const row = candidateRow(
      { endpoint: "p/x@r", total: 0.9, components: { cost: 0.1 }, estimated_cost_usd: 1 },
      { chosen: false, baseline: false, unreachable: false },
    );
    expect(row.inconsistent).toBe(true);
  });
});

describe("money is never fabricated", () => {
  it("renders an unmeasured saving as such, never as $0.00", () => {
    // An unmeasured saving and a saving of zero are different facts, and the
    // gateway is careful about the difference at every layer. A UI that
    // flattened them would put a fabricated zero into screenshots shown to
    // people making purchasing decisions.
    expect(usd(null)).toBe("not measured");
    expect(usd(0)).toBe("$0.000000");
    expect(percent(null)).toBe("—");
  });

  it("renders a negative saving with its sign, because escalation is real", () => {
    // A failed downgrade costs more than not optimizing at all. The gateway
    // records that as a negative saving on purpose; hiding it here would undo
    // the work of disclosing it.
    expect(usd(-0.12)).toBe("-$0.120000");
  });

  it("keeps the realised and counterfactual figures in separate fields", () => {
    const v = verdict(doc);
    // There is no field on Verdict that is the sum of the two, and there must
    // not be: reporting a shadow figure as money already banked is the single
    // most damaging error this product could make.
    expect(Object.keys(v)).not.toContain("totalSaved");
    expect(v.savedUSD === null || typeof v.savedUSD === "number").toBe(true);
  });

  it("reproduces the saving from its own operands", () => {
    const v = verdict(doc);
    if (v.savedUSD === null) return;
    expect(v.baselineCostUSD - v.costUSD).toBeCloseTo(v.savedUSD, 6);
  });
});

describe("an assumed ranking stays honest", () => {
  it("reports which credentials the reader does not actually hold", () => {
    // The fixture is generated with no credentials configured, because that is
    // the state of every machine that has just cloned the repository — and the
    // audience the Explain tab exists for.
    const v = verdict(doc);
    expect(v.assumedCredentials).toBe(true);
    expect(v.missingCredentials.length).toBeGreaterThan(0);
  });
});
