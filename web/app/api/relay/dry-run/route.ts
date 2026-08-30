import { NextRequest } from "next/server";
import { z } from "zod";

import { RELAY_URL, problem, relayHeaders } from "@/lib/relay";
import { DryRun } from "@/lib/types";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

const Body = z.object({
  model: z.string().min(1),
  prompt: z.string().min(1),
  system: z.string().optional(),
  /** Ask Relay to rank as though every credential existed. Dry run only. */
  assumeCredentials: z.boolean().optional(),
  pin: z.enum(["", "strict", "shadow"]).optional(),
});

/**
 * The Explain tab's data source.
 *
 * A dry run calls no provider, spends nothing, and writes no ledger record —
 * Relay answers it before any of that happens. It is therefore the one thing
 * this product can show a stranger at no cost to anybody, which is why it is
 * the console's front door.
 *
 * The response is returned **verbatim**. The dry-run payload *is* the Decision
 * the gateway would act on; a second rendering assembled here would be free to
 * drift from what actually happens on a live request, and an explanation that
 * can disagree with reality is worse than none.
 */
export async function POST(req: NextRequest) {
  let parsed;
  try {
    parsed = Body.parse(await req.json());
  } catch {
    return problem("expected {model, prompt}", 400);
  }

  const headers = relayHeaders(req, { "X-Relay-Dry-Run": "1" });
  if (parsed.assumeCredentials) {
    headers.set("X-Relay-Assume-Credentials", "all");
  }
  if (parsed.pin) headers.set("X-Relay-Pin", parsed.pin);

  const messages = [
    ...(parsed.system ? [{ role: "system", content: parsed.system }] : []),
    { role: "user", content: parsed.prompt },
  ];

  let upstream: Response;
  try {
    upstream = await fetch(`${RELAY_URL}/v1/chat/completions`, {
      method: "POST",
      headers,
      body: JSON.stringify({ model: parsed.model, messages }),
      signal: req.signal,
      cache: "no-store",
    });
  } catch {
    return problem(`cannot reach the gateway at ${RELAY_URL}`, 502);
  }

  const text = await upstream.text();
  if (!upstream.ok) {
    // Relay's own error body is already written for a human and names the
    // remedy — a missing credential, for instance, says which header to send.
    // Passed through rather than paraphrased.
    return new Response(text, {
      status: upstream.status,
      headers: { "Content-Type": "application/json" },
    });
  }

  // Parsed but not reshaped: this validates that the gateway and the console
  // still agree, and fails here with a field name rather than rendering
  // `undefined` somewhere in the browser.
  const result = DryRun.safeParse(JSON.parse(text));
  if (!result.success) {
    return problem(
      `the gateway returned a dry run this console does not understand: ${result.error.issues[0]?.path.join(".")}`,
      502,
    );
  }

  return new Response(text, {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
}
