import { NextRequest } from "next/server";

import { RELAY_ADMIN_URL, problem } from "@/lib/relay";
import { SavingsReport } from "@/lib/types";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

/**
 * The ONLY thing in this system that touches the admin listener.
 *
 * That listener has no authentication until Phase 7 and, asked without a
 * tenant, returns every tenant's spend. It is bound away from the public
 * internet — loopback by default, and in compose it has no published port — and
 * this handler is the single hole deliberately left in that wall.
 *
 * Two rules follow, and neither is optional:
 *
 *  1. A tenant scope is ALWAYS sent. Forwarding a request with none would hand
 *     the caller the whole deployment's cost data.
 *  2. A client-supplied tenant is validated before it is forwarded, never
 *     interpolated as-is.
 */
const TENANT = /^[a-zA-Z0-9_-]{1,64}$/;

export async function GET(req: NextRequest) {
  const requested = req.nextUrl.searchParams.get("tenant") ?? "demo";
  if (!TENANT.test(requested)) {
    return problem("tenant must match [A-Za-z0-9_-]{1,64}", 400);
  }

  const url = `${RELAY_ADMIN_URL}/savings?tenant=${encodeURIComponent(requested)}`;

  let upstream: Response;
  try {
    upstream = await fetch(url, { signal: req.signal, cache: "no-store" });
  } catch {
    return problem(
      `cannot reach the control plane at ${RELAY_ADMIN_URL}. It is bound away from ` +
        `the public internet on purpose; this console reaches it over the private network.`,
      502,
    );
  }

  const text = await upstream.text();
  if (!upstream.ok) {
    return new Response(text, {
      status: upstream.status,
      headers: { "Content-Type": "application/json" },
    });
  }

  const parsed = SavingsReport.safeParse(JSON.parse(text));
  if (!parsed.success) {
    return problem("the control plane returned a report this console does not understand", 502);
  }
  return Response.json(parsed.data);
}
