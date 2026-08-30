import { z } from "zod";

/**
 * Schemas mirroring the Go structs Relay actually serves.
 *
 * Parsed at the proxy boundary rather than trusted, so a version skew between
 * the gateway and this console fails loudly in one place with a message naming
 * the field — instead of rendering `undefined` in six components and being
 * diagnosed in a browser.
 *
 * The shapes are pinned from the Go side too: `TestDryRun_GoldenFixture` writes
 * `fixtures/dryrun.golden.json` from the real handler, and `dryrun.test.ts`
 * parses it with these schemas. A changed struct tag therefore breaks a Go test
 * first, which is the earliest place it can be noticed.
 */

/** One candidate that survived filtering, with the arithmetic that ranked it. */
export const ScoredCandidate = z.object({
  endpoint: z.string(),
  total: z.number(),
  /**
   * Each dimension's *weighted* contribution. These sum to `total`, and showing
   * that they do is the whole point of the ranked table: it answers "why didn't
   * it pick the better model" with arithmetic rather than an assurance.
   */
  components: z.record(z.string(), z.number()).optional(),
  estimated_cost_usd: z.number(),
  reasons: z.array(z.string()).optional(),
});
export type ScoredCandidate = z.infer<typeof ScoredCandidate>;

/** One candidate eliminated by a hard constraint, with the reason. */
export const RejectedCandidate = z.object({
  endpoint: z.string(),
  reason: z.string(),
  detail: z.string().optional(),
});
export type RejectedCandidate = z.infer<typeof RejectedCandidate>;

export const Optimization = z.object({
  lever: z.string(),
  before: z.string(),
  after: z.string(),
  reason: z.string(),
});

export const DryRun = z.object({
  object: z.literal("relay.dry_run"),

  requested_model: z.string(),
  tenant: z.string(),
  route: z.string().optional(),
  mode: z.string(),
  catalog_version: z.string(),
  policy_version: z.string().optional(),

  chosen: z.string(),
  baseline: z.string().optional(),
  substituted: z.boolean(),
  used_fallback: z.boolean().optional(),

  /** Present only in shadow mode: what optimize *would* have chosen. */
  counterfactual: z.string().optional(),

  ranked: z.array(ScoredCandidate).optional(),
  rejected: z.array(RejectedCandidate).optional(),

  optimizations: z.array(Optimization),
  optimizer: z.object({
    outcome: z.string(),
    elapsed_us: z.number(),
    budget_us: z.number(),
    degraded: z.boolean().optional(),
  }),

  cache: z.object({
    eligible: z.boolean(),
    key: z.string().optional(),
    skipped_because: z.string().optional(),
  }),

  credentials: z.object({
    assumed: z.boolean(),
    available: z.array(z.string()),
    missing: z.array(z.string()),
  }),

  estimate: z.object({
    input_tokens_before: z.number(),
    input_tokens_after: z.number(),
    max_output_tokens: z.number(),
    expected_output_tokens: z.number(),
    estimated_cost_usd: z.number(),
    estimated_baseline_cost_usd: z.number(),
    estimated_saved_usd: z.number(),
    /**
     * False means the saving is *unmeasured*, which is a different fact from a
     * saving of zero. The UI must render it as such and never as $0.00.
     */
    saving_measured: z.boolean(),
    cache_breakpoints: z.number(),
  }),

  note: z.string(),
});
export type DryRun = z.infer<typeof DryRun>;

/** Per-dimension totals from the savings ledger. */
export const Totals = z
  .object({
    requests: z.number().optional(),
    cost_micros: z.number().optional(),
    baseline_cost_micros: z.number().optional(),
    /** Money actually saved. Never summed with shadow_saved_micros. */
    saved_micros: z.number().optional(),
    /** Money optimization *would* have saved. Never summed with saved_micros. */
    shadow_saved_micros: z.number().optional(),
    discarded_cost_micros: z.number().optional(),
    substitutions: z.number().optional(),
    escalations: z.number().optional(),
    substitutable_requests: z.number().optional(),
    cache_hits: z.number().optional(),
    input_tokens: z.number().optional(),
    cached_input_tokens: z.number().optional(),
    output_tokens: z.number().optional(),
    breakpoints_inserted: z.number().optional(),
    estimated_usage_requests: z.number().optional(),
  })
  .loose();
export type Totals = z.infer<typeof Totals>;

export const SavingsReport = z
  .object({
    since: z.string().optional(),
    generated_at: z.string().optional(),
    overall: Totals.optional(),
    by_tenant: z.record(z.string(), Totals).optional(),
    by_route: z.record(z.string(), Totals).optional(),
    by_day: z.record(z.string(), Totals).optional(),
    note: z.string().optional(),

    dropped_records: z.number().optional(),
    incomplete: z.boolean().optional(),
    cached_input_share: z.number().optional(),
    response_cache_hit_rate: z.number().optional(),
    escalation_rate: z.number().optional(),
    route_output_p95: z.record(z.string(), z.number()).optional(),
    scope: z.string().optional(),
  })
  .loose();
export type SavingsReport = z.infer<typeof SavingsReport>;

export const ModelList = z.object({
  object: z.string(),
  data: z.array(z.object({ id: z.string() }).loose()),
});
