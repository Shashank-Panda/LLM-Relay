"use client";

import { useEffect, useState } from "react";

import { KeyInputs, useKeyVault } from "@/components/KeyVault";
import { RankedTable } from "@/components/explain/RankedTable";
import { percent, rankedRows, reasonLabel, usd, verdict } from "@/lib/dryrun";
import type { DryRun } from "@/lib/types";

/** One-click scenarios, so a visitor sees the interesting cases without inventing them. */
const PRESETS: { label: string; model: string; prompt: string }[] = [
  {
    label: "Easy classification",
    model: "relay/fast-coder",
    prompt: "Classify this support ticket as billing, bug, or feature request: 'the invoice PDF is blank'.",
  },
  {
    label: "Zero-key demo",
    model: "relay/zero-key-demo",
    prompt: "Write a bash one-liner that counts lines in every .go file.",
  },
  {
    label: "Long context",
    model: "relay/long-context",
    prompt: "Summarise the attached contract in five bullet points.",
  },
];

export function ExplainTab() {
  const vault = useKeyVault();
  const [model, setModel] = useState(PRESETS[0].model);
  const [prompt, setPrompt] = useState(PRESETS[0].prompt);
  const [assume, setAssume] = useState(true);
  const [models, setModels] = useState<string[]>([]);
  const [doc, setDoc] = useState<DryRun | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    fetch("/api/relay/models")
      .then((r) => (r.ok ? r.json() : null))
      .then((j) => {
        if (j?.data) setModels(j.data.map((m: { id: string }) => m.id));
      })
      .catch(() => {});
  }, []);

  async function explain() {
    setBusy(true);
    setError(null);
    try {
      const headers: Record<string, string> = { "Content-Type": "application/json" };
      if (vault.header) headers["X-Relay-Credential"] = vault.header;

      const res = await fetch("/api/relay/dry-run", {
        method: "POST",
        headers,
        body: JSON.stringify({ model, prompt, assumeCredentials: assume }),
      });
      const body = await res.json();
      if (!res.ok) {
        setError(body?.error?.message ?? `the gateway returned ${res.status}`);
        setDoc(null);
        return;
      }
      setDoc(body as DryRun);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      setDoc(null);
    } finally {
      setBusy(false);
    }
  }

  const v = doc ? verdict(doc) : null;
  const rows = doc ? rankedRows(doc, () => undefined) : [];

  return (
    <>
      <div className="panel">
        <h2>Explain a decision — no key, no provider call, no charge</h2>
        <div className="controls" style={{ marginTop: 0, marginBottom: 12 }}>
          {PRESETS.map((p) => (
            <button
              key={p.label}
              type="button"
              className="ghost"
              onClick={() => {
                setModel(p.model);
                setPrompt(p.prompt);
              }}
            >
              {p.label}
            </button>
          ))}
        </div>

        <div className="row">
          <div>
            <label htmlFor="model">Model or route</label>
            <input
              id="model"
              type="text"
              list="models"
              value={model}
              onChange={(e) => setModel(e.target.value)}
            />
            <datalist id="models">
              {models.map((m) => (
                <option key={m} value={m} />
              ))}
            </datalist>
          </div>
        </div>

        <div style={{ marginTop: 12 }}>
          <label htmlFor="prompt">Prompt</label>
          <textarea id="prompt" value={prompt} onChange={(e) => setPrompt(e.target.value)} />
        </div>

        <div className="controls">
          <button className="primary" onClick={explain} disabled={busy || !prompt.trim()}>
            {busy ? "Explaining…" : "Explain"}
          </button>
          <label className="inline">
            <input
              type="checkbox"
              checked={assume}
              onChange={(e) => setAssume(e.target.checked)}
            />
            Rank as if every credential were configured
          </label>
        </div>
        <p className="small muted" style={{ marginBottom: 0 }}>
          A dry run is answered before any provider is contacted, so nothing is billed
          and nothing is written to the savings ledger.
        </p>
      </div>

      {error && <div className="banner bad">{error}</div>}

      {doc && v && (
        <>
          {v.assumedCredentials && v.missingCredentials.length > 0 && (
            <div className="banner info">
              Ranked as though every credential existed. You currently hold keys for{" "}
              <strong>{doc.credentials.available.length}</strong> of{" "}
              <strong>{doc.credentials.available.length + v.missingCredentials.length}</strong>{" "}
              — rows you could not actually reach are marked <span className="chip warn">no key</span>.
            </div>
          )}

          <div className="panel">
            <h2>Verdict</h2>
            <div className="verdict">
              <div>
                <div className="cap">Requested</div>
                <div className="mono">{v.requested}</div>
              </div>
              <div>
                <div className="cap">Served</div>
                <div className="mono">
                  {v.chosen} {v.substituted && <span className="chip sub">substituted</span>}
                  {v.usedFallback && <span className="chip warn">fallback</span>}
                </div>
              </div>
              <div>
                <div className="cap">Estimated saving</div>
                <div className={`fig ${v.savedUSD === null ? "" : v.savedUSD < 0 ? "bad" : "good"}`}>
                  {usd(v.savedUSD)}
                </div>
                <div className="small muted">
                  {usd(v.costUSD)} vs {usd(v.baselineCostUSD)} baseline · {percent(v.savedPercent)}
                </div>
              </div>
              {v.counterfactual && (
                <div>
                  {/* Shadow mode. Labelled as the road not taken and kept in its own
                      column so it can never be read as money already saved. */}
                  <div className="cap">Would have chosen</div>
                  <div className="mono">{v.counterfactual}</div>
                  <div className="small muted">shadow mode — nothing was substituted</div>
                </div>
              )}
            </div>
            <div className="small muted" style={{ marginTop: 12 }}>
              mode <strong>{v.mode}</strong>
              {v.route && <> · route <span className="mono">{v.route}</span></>} · catalog{" "}
              <span className="mono">{v.catalogVersion}</span>
            </div>
          </div>

          <div className="panel">
            <h2>Ranked candidates</h2>
            <RankedTable rows={rows} />
          </div>

          {doc.rejected && doc.rejected.length > 0 && (
            <div className="panel">
              <h2>Eliminated</h2>
              <div className="scroll-x">
                <table>
                  <thead>
                    <tr>
                      <th>Endpoint</th>
                      <th>Reason</th>
                      <th>Detail</th>
                    </tr>
                  </thead>
                  <tbody>
                    {doc.rejected.map((r) => (
                      <tr key={r.endpoint + r.reason}>
                        <td className="mono">{r.endpoint}</td>
                        <td>
                          <span className="chip warn">{reasonLabel(r.reason)}</span>
                        </td>
                        {/* Verbatim. These strings are already written for a human
                            — "quality.coding is 0.85 (asserted 0.90, observed
                            penalty 0.05), floor is 0.80" — and paraphrasing one
                            would lose the numbers that make it useful. */}
                        <td className="small muted">{r.detail}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </div>
          )}

          <div className="panel">
            <h2>Request optimization</h2>
            {doc.optimizer.degraded && (
              <div className="banner warn">
                The optimizer failed open on this request ({doc.optimizer.outcome}). The request was
                still served — but nothing was optimized, and that is silent by design.
              </div>
            )}
            {doc.optimizations.length === 0 ? (
              <p className="small muted" style={{ margin: 0 }}>
                No levers applied ({doc.optimizer.outcome}). Levers are configured per tenant;
                their zero value enables none.
              </p>
            ) : (
              <div className="scroll-x">
                <table>
                  <thead>
                    <tr>
                      <th>Lever</th>
                      <th>Before</th>
                      <th>After</th>
                      <th>Why</th>
                    </tr>
                  </thead>
                  <tbody>
                    {doc.optimizations.map((o) => (
                      <tr key={o.lever}>
                        <td>{o.lever}</td>
                        <td className="mono">{o.before}</td>
                        <td className="mono">{o.after}</td>
                        <td className="small muted">{o.reason}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
            <dl className="kv" style={{ marginTop: 12 }}>
              <dt>optimizer</dt>
              <dd>
                {doc.optimizer.elapsed_us} µs of a {doc.optimizer.budget_us} µs budget
              </dd>
              <dt>tokens</dt>
              <dd>
                {doc.estimate.input_tokens_before} in
                {doc.estimate.input_tokens_after !== doc.estimate.input_tokens_before &&
                  ` → ${doc.estimate.input_tokens_after}`}{" "}
                · {doc.estimate.expected_output_tokens} expected out
              </dd>
              <dt>cache</dt>
              <dd>
                {doc.cache.eligible ? "eligible" : `not cached — ${doc.cache.skipped_because}`}
                {doc.estimate.cache_breakpoints > 0 &&
                  ` · ${doc.estimate.cache_breakpoints} breakpoints inserted`}
              </dd>
            </dl>
          </div>

          {/* Verbatim, and not behind a tooltip. It is three sentences of
              carefully-worded caveat about estimates versus actuals, and a
              screenshot without it is a misleading claim. */}
          <p className="note">{doc.note}</p>
        </>
      )}

      {doc && <KeyInputs missing={doc.credentials.missing} />}
    </>
  );
}
