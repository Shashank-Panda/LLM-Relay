"use client";

import type { CandidateRow } from "@/lib/dryrun";

/**
 * Colour per scoring dimension.
 *
 * Quality dimensions are all one hue because a route ranks on at most one of
 * them, and giving `quality.coding` and `quality.long_context` different colours
 * would imply a comparison that never happens.
 */
function colorFor(dimension: string): string {
  if (dimension.startsWith("quality.")) return "var(--dim-quality)";
  switch (dimension) {
    case "cost":
      return "var(--dim-cost)";
    case "latency":
      return "var(--dim-latency)";
    case "cache_affinity":
      return "var(--dim-cache)";
    default:
      return "var(--dim-other)";
  }
}

/**
 * The ranked candidates, each with its score broken into weighted parts.
 *
 * This table is the reason the Explain tab exists. The gateway's own comment on
 * the field it renders says the components are there to answer "why didn't it
 * pick the better model" *with arithmetic instead of an assurance* — so the
 * arithmetic is what gets drawn: one segment per dimension, each labelled with
 * its weighted contribution, visibly summing to the row's total.
 */
export function RankedTable({ rows }: { rows: CandidateRow[] }) {
  if (rows.length === 0) {
    return <p className="muted small">Nothing survived filtering — see the rejections below.</p>;
  }

  const dimensions = Array.from(
    new Set(rows.flatMap((r) => r.segments.map((s) => s.dimension))),
  );

  return (
    <>
      <div className="scroll-x">
        <table>
          <thead>
            <tr>
              <th>Endpoint</th>
              <th style={{ width: "34%" }}>Score</th>
              <th className="num">Total</th>
              <th className="num">Est. cost</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((r) => (
              <tr key={r.endpoint} className={r.chosen ? "chosen" : undefined}>
                <td>
                  <span className="mono">{r.endpoint}</span>{" "}
                  {r.chosen && <span className="chip sub">served</span>}{" "}
                  {r.baseline && <span className="chip">baseline</span>}{" "}
                  {r.unreachable && (
                    <span className="chip warn" title="You hold no key for this endpoint">
                      no key
                    </span>
                  )}
                  {r.reasons.length > 0 && (
                    <div className="small muted" style={{ marginTop: 4 }}>
                      {r.reasons.join(" · ")}
                    </div>
                  )}
                  {r.inconsistent && (
                    <div className="small" style={{ color: "var(--bad)", marginTop: 4 }}>
                      components do not sum to the total
                    </div>
                  )}
                </td>
                <td>
                  <div className="bar" title={r.segments.map((s) => `${s.dimension} ${s.value.toFixed(3)}`).join("  ")}>
                    {r.segments.map((s) => (
                      <span
                        key={s.dimension}
                        style={{
                          width: `${Math.max(0, s.fraction) * 100}%`,
                          background: colorFor(s.dimension),
                        }}
                      />
                    ))}
                  </div>
                  <div className="small muted" style={{ marginTop: 4 }}>
                    {r.segments.map((s) => `${s.dimension} ${s.value.toFixed(3)}`).join("  +  ")}
                    {r.segments.length > 0 && `  =  ${r.total.toFixed(3)}`}
                  </div>
                </td>
                <td className="num">{r.total.toFixed(3)}</td>
                <td className="num mono">${r.costUSD.toFixed(6)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      {dimensions.length > 0 && (
        <div className="legend">
          {dimensions.map((d) => (
            <span key={d}>
              <span className="sw" style={{ background: colorFor(d) }} />
              {d}
            </span>
          ))}
        </div>
      )}
    </>
  );
}
