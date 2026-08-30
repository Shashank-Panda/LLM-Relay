"use client";

import { useState } from "react";

import { KeyInputs, useKeyVault } from "@/components/KeyVault";
import { usd } from "@/lib/dryrun";

interface Receipt {
  endpoint: string;
  baseline: string;
  mode: string;
  substituted: boolean;
  failover: boolean;
  escalated: string;
  attempts: string;
  cache: string;
  cost: number | null;
  baselineCost: number | null;
  saved: number | null;
  savedEstimated: boolean;
  shadowSaved: number | null;
}

function num(v: string | null): number | null {
  if (v === null || v === "") return null;
  const n = Number(v);
  return Number.isFinite(n) ? n : null;
}

function receiptFrom(h: Headers): Receipt {
  return {
    endpoint: h.get("X-Relay-Endpoint") ?? "",
    baseline: h.get("X-Relay-Baseline") ?? "",
    mode: h.get("X-Relay-Mode") ?? "",
    substituted: h.get("X-Relay-Substituted") === "true",
    failover: h.get("X-Relay-Failover") === "true",
    escalated: h.get("X-Relay-Escalated") ?? "",
    attempts: h.get("X-Relay-Attempts") ?? "",
    cache: h.get("X-Relay-Cache") ?? "",
    cost: num(h.get("X-Relay-Cost-Usd")),
    baselineCost: num(h.get("X-Relay-Baseline-Usd")),
    saved: num(h.get("X-Relay-Saved-Usd")),
    savedEstimated: h.get("X-Relay-Saved-Estimated") === "true",
    shadowSaved: num(h.get("X-Relay-Shadow-Saved-Usd")),
  };
}

export function TryTab() {
  const vault = useKeyVault();
  const [model, setModel] = useState("relay/zero-key-demo");
  const [prompt, setPrompt] = useState("Write a bash one-liner that counts lines in every .go file.");
  const [pin, setPin] = useState<"" | "strict">("");
  const [noCache, setNoCache] = useState(false);
  const [text, setText] = useState("");
  const [receipt, setReceipt] = useState<Receipt | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [missing, setMissing] = useState<string[]>([]);
  const [abort, setAbort] = useState<AbortController | null>(null);

  async function send() {
    const controller = new AbortController();
    setAbort(controller);
    setBusy(true);
    setError(null);
    setText("");
    setReceipt(null);

    try {
      const headers: Record<string, string> = { "Content-Type": "application/json" };
      if (vault.header) headers["X-Relay-Credential"] = vault.header;

      const res = await fetch("/api/relay/chat", {
        method: "POST",
        headers,
        body: JSON.stringify({ model, prompt, stream: true, pin, noCache }),
        signal: controller.signal,
      });

      setReceipt(receiptFrom(res.headers));

      if (!res.ok) {
        const body = await res.json().catch(() => null);
        const message = body?.error?.message ?? `the gateway returned ${res.status}`;
        setError(message);
        // A missing credential names the refs it wants; surface inputs for them.
        const refs = /missing: ([^)]+)\)/.exec(message)?.[1];
        if (refs) setMissing(refs.split(",").map((s) => s.trim()));
        return;
      }

      if (!res.body) {
        setText(await res.text());
        return;
      }

      // Read incrementally. Awaiting .text() here would collapse the stream into
      // a single blob delivered at the end, turning time-to-first-token into
      // time-to-last-token with no error anywhere — the exact failure the
      // gateway's own X-Accel-Buffering header exists to prevent upstream.
      const reader = res.body.getReader();
      const decoder = new TextDecoder();
      let buffer = "";

      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true });

        let cut: number;
        while ((cut = buffer.indexOf("\n\n")) !== -1) {
          const frame = buffer.slice(0, cut);
          buffer = buffer.slice(cut + 2);

          for (const line of frame.split("\n")) {
            // ": …" is a heartbeat comment. The gateway sends them while a model
            // is still thinking, so proxies do not close the connection.
            if (line.startsWith(":") || !line.startsWith("data: ")) continue;
            const payload = line.slice(6);
            if (payload === "[DONE]") continue;

            let chunk: {
              choices?: { delta?: { content?: string } }[];
              error?: { message?: string };
            };
            try {
              chunk = JSON.parse(payload);
            } catch {
              continue;
            }

            // Past the first byte the status code is already sent, so a failure
            // arrives *inside* the stream. A client watching only the HTTP
            // status would show a truncated answer as a success.
            if (chunk.error) {
              setError(chunk.error.message ?? "the stream failed after it started");
              continue;
            }
            const delta = chunk.choices?.[0]?.delta?.content;
            if (delta) setText((t) => t + delta);
          }
        }
      }
    } catch (e) {
      if (e instanceof DOMException && e.name === "AbortError") {
        setError("Cancelled. The gateway propagates that to the provider and stops paying for it.");
      } else {
        setError(e instanceof Error ? e.message : String(e));
      }
    } finally {
      setBusy(false);
      setAbort(null);
    }
  }

  return (
    <>
      <div className="panel">
        <h2>Send a real request</h2>
        <p className="small muted" style={{ marginTop: 0 }}>
          With the compose stack running this needs no API key at all: the demo route is
          served by a free local model against a frontier baseline, so the saving below is
          measured rather than hypothetical.
        </p>

        <div className="row">
          <div>
            <label htmlFor="live-model">Model or route</label>
            <input
              id="live-model"
              type="text"
              value={model}
              onChange={(e) => setModel(e.target.value)}
            />
          </div>
        </div>

        <div style={{ marginTop: 12 }}>
          <label htmlFor="live-prompt">Prompt</label>
          <textarea id="live-prompt" value={prompt} onChange={(e) => setPrompt(e.target.value)} />
        </div>

        <div className="controls">
          <button className="primary" onClick={send} disabled={busy || !prompt.trim()}>
            {busy ? "Streaming…" : "Send"}
          </button>
          {busy && abort && (
            <button className="ghost" type="button" onClick={() => abort.abort()}>
              Cancel
            </button>
          )}
          <label className="inline">
            <input
              type="checkbox"
              checked={pin === "strict"}
              onChange={(e) => setPin(e.target.checked ? "strict" : "")}
            />
            <code>X-Relay-Pin: strict</code>
          </label>
          <label className="inline">
            <input type="checkbox" checked={noCache} onChange={(e) => setNoCache(e.target.checked)} />
            <code>X-Relay-No-Cache</code>
          </label>
        </div>
      </div>

      {error && <div className="banner bad">{error}</div>}

      {receipt && receipt.endpoint && (
        <div className="panel">
          <h2>What actually happened</h2>
          <div className="verdict">
            <div>
              <div className="cap">Served</div>
              <div className="mono">
                {receipt.endpoint}{" "}
                {/* Substitution and failover are visually distinct on purpose: a
                    recovery onto another region running the model you asked for
                    is not a downgrade, and reporting it as one would tell a
                    caller they were served something lesser when they were not. */}
                {receipt.substituted && <span className="chip sub">substituted</span>}
                {receipt.failover && <span className="chip warn">failover</span>}
                {receipt.escalated && <span className="chip bad">escalated: {receipt.escalated}</span>}
              </div>
              <div className="small muted">baseline {receipt.baseline || "—"}</div>
            </div>
            <div>
              <div className="cap">Saved</div>
              <div className={`fig ${receipt.saved === null ? "" : receipt.saved < 0 ? "bad" : "good"}`}>
                {usd(receipt.saved)}
              </div>
              <div className="small muted">
                {usd(receipt.cost)} vs {usd(receipt.baselineCost)} baseline
                {receipt.savedEstimated && (
                  <>
                    {" "}
                    <span className="chip warn" title="Headers are fixed before the first byte; token counts arrive at the end">
                      estimated
                    </span>
                  </>
                )}
              </div>
            </div>
            {receipt.shadowSaved !== null && (
              <div>
                <div className="cap">Would have saved</div>
                <div className="fig">{usd(receipt.shadowSaved)}</div>
                <div className="small muted">shadow mode — never added to the figure on the left</div>
              </div>
            )}
          </div>
          <div className="small muted" style={{ marginTop: 12 }}>
            mode <strong>{receipt.mode}</strong>
            {receipt.attempts && <> · {receipt.attempts} attempt(s)</>}
            {receipt.cache && <> · cache: {receipt.cache}</>}
          </div>
        </div>
      )}

      {(text || busy) && (
        <div className="panel">
          <h2>Response</h2>
          <pre className="stream">{text || (busy ? "…" : "")}</pre>
        </div>
      )}

      <KeyInputs missing={missing} />
    </>
  );
}
