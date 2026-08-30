"use client";

import { createContext, useContext, useMemo, useState } from "react";

/**
 * Caller-supplied provider keys, held in React state and nowhere else.
 *
 * Never localStorage, never sessionStorage, never IndexedDB, never a cookie. A
 * reload loses them, and that is the correct behaviour rather than a
 * limitation: a demo that persists a stranger's provider key has created a
 * liability the first time someone shares a laptop, and nothing here is worth
 * that.
 *
 * The keys are sent only to this console's own origin, as headers, on their way
 * to Relay. They are never placed in a URL or a query string, where they would
 * end up in access logs and browser history.
 */
export interface KeyVault {
  keys: Record<string, string>;
  set: (ref: string, secret: string) => void;
  clear: () => void;
  /** Header value per supplied ref, newline-joined for the proxy to split. */
  header: string | null;
}

const Ctx = createContext<KeyVault | null>(null);

export function KeyVaultProvider({ children }: { children: React.ReactNode }) {
  const [keys, setKeys] = useState<Record<string, string>>({});

  const value = useMemo<KeyVault>(() => {
    const entries = Object.entries(keys).filter(([, v]) => v.trim() !== "");
    return {
      keys,
      set: (ref, secret) => setKeys((k) => ({ ...k, [ref]: secret })),
      clear: () => setKeys({}),
      header: entries.length
        ? entries.map(([ref, secret]) => `${ref} ${secret.trim()}`).join("\n")
        : null,
    };
  }, [keys]);

  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useKeyVault(): KeyVault {
  const v = useContext(Ctx);
  if (!v) throw new Error("useKeyVault outside KeyVaultProvider");
  return v;
}

/** Inputs for the credential refs the gateway says the caller is missing. */
export function KeyInputs({ missing }: { missing: string[] }) {
  const vault = useKeyVault();
  if (missing.length === 0) return null;

  return (
    <div className="panel">
      <h2>Your provider keys</h2>
      <p className="small muted" style={{ marginTop: 0 }}>
        Held in this tab only. They are gone when you reload, are never written to
        storage, and are forwarded to the gateway for the one request that uses
        them. Relay does not store, log, or record them in its savings ledger.
      </p>
      <div className="row">
        {missing.map((ref) => (
          <div key={ref}>
            <label htmlFor={`key-${ref}`}>{ref}</label>
            <input
              id={`key-${ref}`}
              type="password"
              autoComplete="off"
              data-1p-ignore="true"
              data-lpignore="true"
              spellCheck={false}
              placeholder="sk-…"
              value={vault.keys[ref] ?? ""}
              onChange={(e) => vault.set(ref, e.target.value)}
            />
          </div>
        ))}
      </div>
      {vault.header && (
        <div className="controls">
          <button className="ghost" onClick={vault.clear} type="button">
            Clear keys
          </button>
          <span className="small muted">
            Sent through this console&rsquo;s server on the way to the gateway. On your own
            machine both are yours; on a hosted deployment that is a real trust
            delegation and this is where it is disclosed.
          </span>
        </div>
      )}
    </div>
  );
}
