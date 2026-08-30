import { NextRequest } from "next/server";
import { z } from "zod";

import { RELAY_URL, pickDisclosure, problem, relayHeaders } from "@/lib/relay";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

const Body = z.object({
  model: z.string().min(1),
  prompt: z.string().min(1),
  system: z.string().optional(),
  stream: z.boolean().optional(),
  pin: z.enum(["", "strict", "shadow"]).optional(),
  noCache: z.boolean().optional(),
});

/**
 * The Try-live tab's data source: a real request, with the caller's own key if
 * they supplied one.
 *
 * Three details here are worth more than they look.
 *
 * The upstream body is passed through **unbuffered**. Anything that awaits
 * `.text()` collapses the stream into one blob delivered at the end, which
 * turns time-to-first-token into time-to-last-token and silently destroys the
 * single most persuasive moment of the demo, with no error anywhere.
 *
 * `req.signal` is forwarded, so a browser abort propagates. Relay already
 * cancels the provider call when its client disconnects — that is one of Phase
 * 1's "done when" criteria — and forwarding the signal is what lets the console
 * actually demonstrate it.
 *
 * Disclosure headers are copied by allowlist. They carry the product's entire
 * claim, so they must survive the proxy hop; copying everything would forward
 * whatever a future gateway version adds, including things meant to stay on the
 * private network.
 */
export async function POST(req: NextRequest) {
  let parsed;
  try {
    parsed = Body.parse(await req.json());
  } catch {
    return problem("expected {model, prompt}", 400);
  }

  const headers = relayHeaders(req);
  if (parsed.pin) headers.set("X-Relay-Pin", parsed.pin);
  if (parsed.noCache) headers.set("X-Relay-No-Cache", "1");

  const messages = [
    ...(parsed.system ? [{ role: "system", content: parsed.system }] : []),
    { role: "user", content: parsed.prompt },
  ];

  let upstream: Response;
  try {
    upstream = await fetch(`${RELAY_URL}/v1/chat/completions`, {
      method: "POST",
      headers,
      body: JSON.stringify({
        model: parsed.model,
        messages,
        stream: parsed.stream ?? true,
      }),
      signal: req.signal,
      cache: "no-store",
    });
  } catch {
    return problem(`cannot reach the gateway at ${RELAY_URL}`, 502);
  }

  const out = pickDisclosure(upstream.headers);
  out.set("Content-Type", upstream.headers.get("Content-Type") ?? "application/json");
  // Without this an intermediary may buffer the whole stream and deliver it at
  // once, which is the same failure as awaiting .text() but caused by somebody
  // else's proxy.
  out.set("Cache-Control", "no-cache, no-transform");
  out.set("X-Accel-Buffering", "no");

  return new Response(upstream.body, { status: upstream.status, headers: out });
}
