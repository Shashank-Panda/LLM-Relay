import "server-only";

/**
 * The only module that knows where Relay lives.
 *
 * `server-only` is not decoration: RELAY_ADMIN_URL points at the control plane,
 * which has no authentication until Phase 7 and returns every tenant's spend
 * when unscoped. If a refactor ever pulls this file into a client bundle the
 * build fails here rather than shipping that address to a browser.
 *
 * Nothing in this file logs a request body or a header. A caller's provider key
 * passes through this process on its way to Relay, and the gateway's own access
 * log is deliberately limited to method, path, status, bytes and duration for
 * exactly the same reason. Keep it that way.
 */

export const RELAY_URL = process.env.RELAY_URL ?? "http://localhost:8080";
export const RELAY_ADMIN_URL = process.env.RELAY_ADMIN_URL ?? "http://localhost:9090";

/**
 * The Relay tenant key the console authenticates its own traffic with.
 *
 * A *tenant* key, not a provider key — the two authenticate different things to
 * different parties. It has to name a tenant in optimize mode or the Explain tab
 * cannot demonstrate substitution, because pinning `optimize` on a request is
 * deliberately not honoured: that permission belongs to the tenant.
 */
export const DEMO_TENANT_KEY = process.env.RELAY_DEMO_TENANT_KEY ?? "";

/** The header carrying one caller-supplied provider key: "<ref> <secret>". */
export const CREDENTIAL_HEADER = "X-Relay-Credential";

/**
 * Disclosure headers copied back to the browser, by allowlist.
 *
 * An allowlist rather than a blanket copy. Relay's product claim lives in these
 * headers so they must survive the proxy hop, but forwarding everything would
 * mean forwarding whatever a future version of the gateway adds — including
 * something that was never meant to leave the private network.
 */
export const DISCLOSURE_HEADERS = [
  "X-Relay-Endpoint",
  "X-Relay-Baseline",
  "X-Relay-Mode",
  "X-Relay-Substituted",
  "X-Relay-Catalog-Version",
  "X-Relay-Decision",
  "X-Relay-Cost-Usd",
  "X-Relay-Baseline-Usd",
  "X-Relay-Saved-Usd",
  "X-Relay-Saved-Estimated",
  "X-Relay-Shadow-Saved-Usd",
  "X-Relay-Optimizations",
  "X-Relay-Cache",
  "X-Relay-Attempts",
  "X-Relay-Rerouted",
  "X-Relay-Failover",
  "X-Relay-Escalated",
  "X-Request-Id",
] as const;

/**
 * credentialHeaders extracts the caller's provider keys from an inbound console
 * request so they can be forwarded verbatim.
 *
 * Forwarded, never stored. This process holds them for the duration of one
 * fetch and nothing writes them anywhere.
 */
export function credentialHeaders(req: Request): [string, string][] {
  const out: [string, string][] = [];
  // getSetCookie-style multi-value access is not available for arbitrary
  // headers, and Relay accepts a repeated header, so the browser sends them
  // newline-joined and they are split back out here.
  const raw = req.headers.get(CREDENTIAL_HEADER);
  if (!raw) return out;
  for (const line of raw.split("\n")) {
    const v = line.trim();
    if (v) out.push([CREDENTIAL_HEADER, v]);
  }
  return out;
}

/** Copies the allowlisted disclosure headers from an upstream response. */
export function pickDisclosure(from: Headers): Headers {
  const h = new Headers();
  for (const name of DISCLOSURE_HEADERS) {
    const v = from.get(name);
    if (v !== null) h.set(name, v);
  }
  return h;
}

/** Builds the header set for a call to the gateway's data plane. */
export function relayHeaders(req: Request, extra: Record<string, string> = {}): Headers {
  const h = new Headers({ "Content-Type": "application/json" });
  if (DEMO_TENANT_KEY) h.set("Authorization", `Bearer ${DEMO_TENANT_KEY}`);
  for (const [k, v] of Object.entries(extra)) h.set(k, v);
  for (const [k, v] of credentialHeaders(req)) h.append(k, v);
  return h;
}

/** A JSON error in the shape the console's own components expect. */
export function problem(message: string, status: number): Response {
  return Response.json({ error: { message } }, { status });
}
