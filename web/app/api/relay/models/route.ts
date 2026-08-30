import { NextRequest } from "next/server";

import { RELAY_URL, problem } from "@/lib/relay";
import { ModelList } from "@/lib/types";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

/**
 * Populates the model picker from the catalog the gateway is actually running,
 * so the console never hardcodes a model name and cannot drift from the
 * deployment it is pointed at.
 */
export async function GET(req: NextRequest) {
  let upstream: Response;
  try {
    upstream = await fetch(`${RELAY_URL}/v1/models`, { signal: req.signal, cache: "no-store" });
  } catch {
    return problem(`cannot reach the gateway at ${RELAY_URL}`, 502);
  }

  const text = await upstream.text();
  if (!upstream.ok) {
    return new Response(text, {
      status: upstream.status,
      headers: { "Content-Type": "application/json" },
    });
  }

  const parsed = ModelList.safeParse(JSON.parse(text));
  if (!parsed.success) {
    return problem("the gateway returned a model list this console does not understand", 502);
  }
  return Response.json(parsed.data);
}
