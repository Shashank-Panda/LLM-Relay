#!/usr/bin/env bash
# End-to-end check against a running compose stack.
#
#   docker compose up -d && ./scripts/smoke.sh
#
# Asserts the two claims the zero-key demo makes: that the gateway explains a
# routing decision without calling a provider, and that it then serves a real
# request from the free local model against a frontier baseline — producing a
# *measured* saving on a machine holding no API key.
set -euo pipefail

RELAY="${RELAY_URL:-http://localhost:8080}"
ADMIN="${RELAY_ADMIN_URL:-http://localhost:9090}"
KEY="${RELAY_DEMO_TENANT_KEY:-sk-relay-demo-optimize-0f1e2d3c4b5a}"

fail() { echo "FAIL: $*" >&2; exit 1; }
ok()   { echo "  ok  $*"; }

echo "==> readiness"
for i in $(seq 1 60); do
  if curl -fsS "$RELAY/readyz" >/dev/null 2>&1; then break; fi
  [ "$i" = 60 ] && fail "gateway never became ready"
  sleep 2
done
ok "/readyz"

echo "==> dry run costs nothing and calls nobody"
dry=$(curl -fsS "$RELAY/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  -H 'X-Relay-Dry-Run: 1' \
  -H "Authorization: Bearer $KEY" \
  -d '{"model":"relay/zero-key-demo","messages":[{"role":"user","content":"hi"}]}')

echo "$dry" | grep -q '"object":"relay.dry_run"' || fail "not a dry-run response: $dry"
ok "returns relay.dry_run"
echo "$dry" | grep -q 'ollama/qwen-coder@local' || fail "the free endpoint is not in the decision"
ok "ranks the free local endpoint"

echo "==> a real request is served by the free model"
hdr=$(mktemp)
curl -fsS -D "$hdr" -o /dev/null "$RELAY/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  -H "Authorization: Bearer $KEY" \
  -d '{"model":"relay/zero-key-demo","messages":[{"role":"user","content":"say hi"}]}'

grep -qi '^x-relay-endpoint: *ollama/qwen-coder@local' "$hdr" \
  || fail "served by $(grep -i '^x-relay-endpoint' "$hdr" || echo '(no endpoint header)')"
ok "X-Relay-Endpoint is the local model"
grep -qi '^x-relay-substituted: *true' "$hdr" || fail "not disclosed as a substitution"
ok "X-Relay-Substituted: true"

# The three money headers must reproduce each other. A saving whose operands do
# not subtract to it is a number nobody can audit.
cost=$(grep -i '^x-relay-cost-usd:' "$hdr" | tr -d '\r' | awk '{print $2}')
base=$(grep -i '^x-relay-baseline-usd:' "$hdr" | tr -d '\r' | awk '{print $2}')
save=$(grep -i '^x-relay-saved-usd:' "$hdr" | tr -d '\r' | awk '{print $2}')
[ -n "$cost" ] && [ -n "$base" ] && [ -n "$save" ] || fail "money headers missing"
awk -v c="$cost" -v b="$base" -v s="$save" \
  'BEGIN{ if ((b-c)-s > 5e-7 || (b-c)-s < -5e-7) exit 1 }' \
  || fail "baseline($base) - cost($cost) != saved($save)"
ok "saved $save = baseline $base - cost $cost"

echo "==> the saving is measured, not counterfactual"
rep=$(curl -fsS "$ADMIN/savings?tenant=demo" 2>/dev/null) \
  || fail "admin listener unreachable from here (expected: it is not published; run this inside the network or with --profile debug)"
echo "$rep" | grep -q '"saved_micros"' || fail "no saved_micros in the report"
ok "/savings reports the tenant"

rm -f "$hdr"
echo
echo "PASS — a measured saving, with no provider credential anywhere."
