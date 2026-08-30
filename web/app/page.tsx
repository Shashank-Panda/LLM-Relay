"use client";

import { useState } from "react";

import { KeyVaultProvider } from "@/components/KeyVault";
import { ExplainTab } from "@/components/explain/ExplainTab";
import { TryTab } from "@/components/live/TryTab";
import { SavingsTab } from "@/components/savings/SavingsTab";

const TABS = [
  { id: "explain", label: "Explain", hint: "no key, no charge" },
  { id: "try", label: "Try live", hint: "a real request" },
  { id: "savings", label: "Savings", hint: "the ledger" },
] as const;

type TabID = (typeof TABS)[number]["id"];

export default function Page() {
  const [tab, setTab] = useState<TabID>("explain");

  return (
    <KeyVaultProvider>
      <div className="shell">
        <header className="masthead">
          <h1>Relay</h1>
          <span className="sub">
            Cut your LLM bill without changing your code — and see exactly why.
          </span>
        </header>

        <nav className="tabs" role="tablist">
          {TABS.map((t) => (
            <button
              key={t.id}
              role="tab"
              aria-selected={tab === t.id}
              onClick={() => setTab(t.id)}
            >
              {t.label} <span className="muted small">· {t.hint}</span>
            </button>
          ))}
        </nav>

        {tab === "explain" && <ExplainTab />}
        {tab === "try" && <TryTab />}
        {tab === "savings" && <SavingsTab />}
      </div>
    </KeyVaultProvider>
  );
}
