import type { DryRun, ScoredCandidate } from "./types";

/**
 * Pure mapping from Relay's dry-run payload to what the Explain tab draws.
 *
 * Kept out of the components so it can be tested against the fixture the Go
 * suite generates, and so the three rules that must never be broken by a UI are
 * expressed once, in code, rather than remembered in three render functions:
 *
 *   - a saving that is *unmeasured* is not a saving of zero;
 *   - a shadow figure is never added to a realised one;
 *   - the score components sum to the total, and are shown doing so.
 */

/** One segment of a candidate's stacked score bar. */
export interface Segment {
  dimension: string;
  value: number;
  /** Share of the row's total, 0..1, for width. */
  fraction: number;
}

export interface CandidateRow {
  endpoint: string;
  total: number;
  costUSD: number;
  reasons: string[];
  segments: Segment[];
  /** True when the segments do not reproduce the total. */
  inconsistent: boolean;
  chosen: boolean;
  baseline: boolean;
  /** The caller holds no credential for this endpoint's provider. */
  unreachable: boolean;
}

const EPSILON = 5e-4;

/**
 * Turns a candidate into a row whose bar segments visibly sum to its total.
 *
 * `inconsistent` is surfaced rather than hidden. The components are the
 * gateway's own arithmetic; if they stop adding up, the honest thing is to say
 * so on the row, because the alternative is a chart that looks right and is not.
 */
export function candidateRow(
  c: ScoredCandidate,
  opts: { chosen: boolean; baseline: boolean; unreachable: boolean },
): CandidateRow {
  const entries = Object.entries(c.components ?? {}).filter(([, v]) => v !== 0);
  entries.sort((a, b) => b[1] - a[1]);

  const sum = entries.reduce((acc, [, v]) => acc + v, 0);
  const denom = sum === 0 ? 1 : sum;

  return {
    endpoint: c.endpoint,
    total: c.total,
    costUSD: c.estimated_cost_usd,
    reasons: c.reasons ?? [],
    segments: entries.map(([dimension, value]) => ({
      dimension,
      value,
      fraction: value / denom,
    })),
    inconsistent: entries.length > 0 && Math.abs(sum - c.total) > EPSILON,
    ...opts,
  };
}

export interface Verdict {
  requested: string;
  chosen: string;
  baseline?: string;
  substituted: boolean;
  usedFallback: boolean;
  mode: string;
  route?: string;
  catalogVersion: string;

  /** Realised saving. Null when the saving is unmeasured. */
  savedUSD: number | null;
  costUSD: number;
  baselineCostUSD: number;
  /** Percentage of the baseline saved, or null when unmeasured. */
  savedPercent: number | null;

  /**
   * The road not taken, in shadow mode only: what optimize would have chosen
   * and what it would have saved. Held in its own fields so that no caller can
   * add it to savedUSD by accident — the two are different claims and summing
   * them tells a customer they banked money they did not.
   */
  counterfactual?: string;

  /** True when the ranking assumed credentials the reader may not hold. */
  assumedCredentials: boolean;
  missingCredentials: string[];
}

export function verdict(d: DryRun): Verdict {
  const e = d.estimate;
  const measured = e.saving_measured;
  const baselineCost = e.estimated_baseline_cost_usd;

  return {
    requested: d.requested_model,
    chosen: d.chosen,
    baseline: d.baseline,
    substituted: d.substituted,
    usedFallback: d.used_fallback ?? false,
    mode: d.mode,
    route: d.route,
    catalogVersion: d.catalog_version,

    savedUSD: measured ? e.estimated_saved_usd : null,
    costUSD: e.estimated_cost_usd,
    baselineCostUSD: baselineCost,
    savedPercent:
      measured && baselineCost > 0 ? (e.estimated_saved_usd / baselineCost) * 100 : null,

    counterfactual: d.counterfactual,
    assumedCredentials: d.credentials.assumed,
    missingCredentials: d.credentials.missing,
  };
}

/**
 * Builds the ranked rows, marking which the caller could not actually reach.
 *
 * `unreachable` is what keeps an assumed ranking honest: the arithmetic is over
 * the whole catalog so the reader sees the real comparison, and the rows they
 * hold no key for are marked rather than removed.
 */
export function rankedRows(d: DryRun, credentialRefFor: (endpoint: string) => string | undefined): CandidateRow[] {
  const missing = new Set(d.credentials.missing);
  return (d.ranked ?? []).map((c) =>
    candidateRow(c, {
      chosen: c.endpoint === d.chosen,
      baseline: c.endpoint === d.baseline,
      unreachable: missing.has(credentialRefFor(c.endpoint) ?? ""),
    }),
  );
}

/**
 * Formats a dollar figure, or the reason there isn't one.
 *
 * `null` renders as "not measured" and never as "$0.00". The distinction is one
 * the gateway is careful about at every layer — an unmeasured saving is not a
 * saving of zero — and a UI that flattened it would put a fabricated zero into
 * screenshots that get shown to buyers.
 */
export function usd(v: number | null, digits = 6): string {
  if (v === null) return "not measured";
  const sign = v < 0 ? "-" : "";
  return `${sign}$${Math.abs(v).toFixed(digits)}`;
}

export function percent(v: number | null): string {
  return v === null ? "—" : `${v.toFixed(1)}%`;
}

/** Short label for a reject reason, for a chip. */
export function reasonLabel(reason: string): string {
  return reason.replace(/([a-z])([A-Z])/g, "$1 $2");
}
