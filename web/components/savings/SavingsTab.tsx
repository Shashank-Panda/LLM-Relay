"use client";

import { useCallback, useEffect, useState } from "react";

import type { SavingsReport, Totals } from "@/lib/types";

/** Micro-dollars to dollars. The ledger stores integers so money never rounds. */
function dollars(micros: number | undefined): number {
  return (micros ?? 0) / 1e6;
}

function fmt(v: number): string {
  const sign = v < 0 ? "-" : "";
  return `${sign}$${Math.abs(v).toFixed(6)}`;
}

function TotalsTable({ title, rows }: { title: string; rows: [string, Totals][] }) {
  if (rows.length === 0) return null;
  return (
    <div className="panel">
      <h2>{title}</h2>
      <div className="scroll-x">
        <table>
          <thead>
            <tr>
              <th>{title.replace(/^By /, "")}</th>
              <th className="num">Requests</th>
              <th className="num">Cost</th>
              <th className="num">Baseline</th>
              {/* Two columns, never one. See the note in the component below. */}
              <th className="num">Saved</th>
              <th className="num">Would have saved</th>
            </tr>
          </thead>
          <tbody>
            {rows.map(([key, t]) => (
              <tr key={key}>
                <td className="mono">{key}</td>
                <td className="num">{t.requests ?? 0}</td>
                <td className="num mono">{fmt(dollars(t.cost_micros))}</td>
                <td className="num mono">{fmt(dollars(t.baseline_cost_micros))}</td>
                <td className="num mono" style={{ color: "var(--good)" }}>
                  {fmt(dollars(t.saved_micros))}
                </td>
                <td className="num mono muted">{fmt(dollars(t.shadow_saved_micros))}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

/** Fetches one tenant's report, or throws with a message worth showing. */
async function fetchReport(tenant: string, signal?: AbortSignal): Promise<SavingsReport> {
  const res = await fetch(`/api/relay/savings?tenant=${encodeURIComponent(tenant)}`, { signal });
  const body = await res.json();
  if (!res.ok) {
    throw new Error(body?.error?.message ?? `the control plane returned ${res.status}`);
  }
  return body as SavingsReport;
}

export function SavingsTab() {
  const [tenant, setTenant] = useState("demo");
  const [report, setReport] = useState<SavingsReport | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  // Explicit refresh, for the button.
  const load = useCallback(async () => {
    setBusy(true);
    setError(null);
    try {
      setReport(await fetchReport(tenant));
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      setReport(null);
    } finally {
      setBusy(false);
    }
  }, [tenant]);

  // Load on mount and whenever the tenant changes. State is only touched in the
  // async continuations, and the in-flight request is aborted when the tenant
  // changes again — otherwise a slow response for the previous tenant can land
  // after a fast one for the current tenant and quietly show the wrong figures.
  useEffect(() => {
    const ctrl = new AbortController();
    fetchReport(tenant, ctrl.signal)
      .then((r) => setReport(r))
      .catch((e: unknown) => {
        if (e instanceof DOMException && e.name === "AbortError") return;
        setError(e instanceof Error ? e.message : String(e));
      });
    return () => ctrl.abort();
  }, [tenant]);

  const overall = report?.overall;
  const requests = overall?.requests ?? 0;

  return (
    <>
      <div className="panel">
        <h2>Savings report</h2>
        <div className="controls" style={{ marginTop: 0 }}>
          <div style={{ flex: "0 1 240px" }}>
            <label htmlFor="tenant">Tenant</label>
            <input id="tenant" type="text" value={tenant} onChange={(e) => setTenant(e.target.value)} />
          </div>
          <button className="primary" onClick={load} disabled={busy}>
            {busy ? "Loading…" : "Refresh"}
          </button>
        </div>
      </div>

      {error && <div className="banner bad">{error}</div>}

      {report && (
        <>
          {/* A hole in the ledger, reported prominently rather than as a
              footnote. A report that does not admit to one overstates its own
              completeness — the gateway says so in its own comment, and a UI
              that buried it would undo that. */}
          {report.incomplete && (
            <div className="banner warn">
              This report is incomplete: {report.dropped_records} ledger record(s) were dropped
              under load. The figures below understate what happened.
            </div>
          )}

          {requests === 0 && (
            <div className="banner info">
              No requests recorded for <span className="mono">{tenant}</span> yet. Try the
              <strong> Try live</strong> tab — dry runs are deliberately never recorded, because
              nothing was spent.
            </div>
          )}

          <div className="panel">
            <h2>Overall</h2>
            <div className="verdict">
              <div>
                <div className="cap">Requests</div>
                <div className="fig">{requests}</div>
              </div>
              <div>
                <div className="cap">Cost</div>
                <div className="fig mono">{fmt(dollars(overall?.cost_micros))}</div>
                <div className="small muted">
                  baseline {fmt(dollars(overall?.baseline_cost_micros))}
                </div>
              </div>
              <div>
                <div className="cap">Saved</div>
                <div className="fig good">{fmt(dollars(overall?.saved_micros))}</div>
                <div className="small muted">money not spent</div>
              </div>
              <div>
                {/*
                  Deliberately a separate figure, in a different colour, with no
                  total anywhere that adds it to the one on its left. In shadow
                  mode the baseline *is* served, so nothing was saved and this is
                  what optimization would have saved. Presenting the second as
                  the first would tell a customer they had banked money they had
                  not, which the roadmap calls the single most damaging error
                  this product could make.
                */}
                <div className="cap">Would have saved</div>
                <div className="fig muted">{fmt(dollars(overall?.shadow_saved_micros))}</div>
                <div className="small muted">counterfactual — not banked</div>
              </div>
            </div>

            <dl className="kv" style={{ marginTop: 16 }}>
              <dt>substitutions</dt>
              <dd>
                {overall?.substitutions ?? 0} of {overall?.substitutable_requests ?? 0} eligible
              </dd>
              <dt>escalations</dt>
              <dd>
                {overall?.escalations ?? 0}
                {report.escalation_rate !== undefined &&
                  ` (${(report.escalation_rate * 100).toFixed(2)}% — SLO is 2%)`}
                {(overall?.discarded_cost_micros ?? 0) > 0 && (
                  <> · {fmt(dollars(overall?.discarded_cost_micros))} paid for and discarded</>
                )}
              </dd>
              <dt>cached input</dt>
              <dd>
                {((report.cached_input_share ?? 0) * 100).toFixed(1)}% of purchased input tokens
                {(overall?.breakpoints_inserted ?? 0) > 0 &&
                  ` · ${overall?.breakpoints_inserted} breakpoints inserted`}
              </dd>
              <dt>response cache</dt>
              <dd>
                {overall?.cache_hits ?? 0} hits ·{" "}
                {((report.response_cache_hit_rate ?? 0) * 100).toFixed(1)}% hit rate
              </dd>
            </dl>
          </div>

          <TotalsTable title="By route" rows={Object.entries(report.by_route ?? {})} />
          <TotalsTable title="By day" rows={Object.entries(report.by_day ?? {})} />

          {report.note && <p className="note">{report.note}</p>}
          {/* Verbatim and in the open: it says this is one process's view since
              boot, which is exactly the caveat somebody quoting the number needs
              and exactly the one a tooltip would hide. */}
          {report.scope && <p className="note">{report.scope}</p>}
        </>
      )}
    </>
  );
}
