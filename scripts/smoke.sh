#!/usr/bin/env bash
# End-to-end smoke test against a running stack that uses the mock provider:
#
#   docker compose up -d --build --wait && ./scripts/smoke.sh
#
# Checks the main promises of the system: notifications are delivered,
# idempotency holds, validation works, and a provider outage pauses
# delivery instead of failing messages. Needs curl and jq.
set -euo pipefail

API=${API:-http://localhost:8080}
MOCK=${MOCK:-http://localhost:8090}
WORKER=${WORKER:-http://localhost:9091}
JSON='Content-Type: application/json'

pass() { printf '  \033[32m✓\033[0m %s\n' "$1"; }
fail() { printf '  \033[31m✗ %s\033[0m\n' "$1"; exit 1; }

# wait_batch BATCH_ID TIMEOUT_SECONDS: waits until nothing in the batch is
# pending or processing, then prints its status counts.
wait_batch() {
  local counts
  for _ in $(seq 1 "$2"); do
    counts=$(curl -sf "$API/v1/batches/$1" | jq -c .status_counts)
    if jq -e '(.pending // 0) + (.processing // 0) == 0' <<<"$counts" >/dev/null; then
      echo "$counts"; return 0
    fi
    sleep 1
  done
  echo "$counts"; return 1
}

batch() { # batch COUNT CHANNEL PREFIX: creates a batch, prints its ID
  local recipient
  case $2 in
    sms) recipient='"+9055\(10000000 + .)"' ;;
    email) recipient='"u\(.)@example.com"' ;;
    push) recipient='"token-\(.)"' ;;
  esac
  jq -nc "{notifications: [range($1) | {recipient: $recipient, channel: \"$2\", content: \"$3 \(.)\"}]}" |
    curl -sf -X POST "$API/v1/notifications/batch" -H "$JSON" -d @- | jq -r .batch_id
}

echo "Smoke test against $API"
curl -sf -X PUT "$MOCK/admin/config" -d '{"failure_rate":0.1,"failure_status":500}' >/dev/null

echo "Health"
[[ $(curl -sf "$API/readyz" | jq -r .status) == ok ]] && pass "API ready" || fail "API not ready"
[[ $(curl -sf "$WORKER/readyz" | jq -r .status) == ok ]] && pass "worker ready" || fail "worker not ready"

echo "Idempotency and validation"
key="smoke-$(date +%s%N)"
body='{"recipient":"+905551234567","channel":"sms","content":"smoke","priority":"high"}'
first=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$API/v1/notifications" -H "$JSON" -H "Idempotency-Key: $key" -d "$body")
second=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$API/v1/notifications" -H "$JSON" -H "Idempotency-Key: $key" -d "$body")
[[ $first == 201 && $second == 200 ]] && pass "same Idempotency-Key: 201 then 200" || fail "idempotency: got $first then $second"
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$API/v1/notifications" -H "$JSON" -d '{"recipient":"x","channel":"sms","content":""}')
[[ $code == 422 ]] && pass "invalid input rejected with 422" || fail "validation: got $code"

echo "Delivery (10% simulated provider failures)"
ids=()
for ch in sms email push; do ids+=("$(batch 100 $ch delivery)"); done
for id in "${ids[@]}"; do
  counts=$(wait_batch "$id" 60) || fail "batch $id did not finish: $counts"
  [[ $(jq '.sent // 0' <<<"$counts") == 100 ]] && pass "batch of 100 delivered: $counts" || fail "batch $id: $counts"
done

echo "Provider outage"
curl -sf -X PUT "$MOCK/admin/config" -d '{"failure_rate":1,"failure_status":503}' >/dev/null
id=$(batch 50 sms outage)
sleep 8
[[ $(curl -sf "$WORKER/breakers" | jq -r .sms) != closed ]] && pass "circuit breaker opened" || fail "breaker did not open"
counts=$(curl -sf "$API/v1/batches/$id" | jq -c .status_counts)
[[ $(jq '.failed // 0' <<<"$counts") == 0 ]] && pass "nothing failed during the outage: $counts" || fail "messages failed during outage: $counts"
curl -sf -X PUT "$MOCK/admin/config" -d '{"failure_rate":0}' >/dev/null
counts=$(wait_batch "$id" 90) || fail "batch did not drain after recovery: $counts"
[[ $(jq '.sent // 0' <<<"$counts") == 50 ]] && pass "all delivered after recovery: $counts" || fail "after recovery: $counts"

echo "Metrics"
sent=$(curl -sf "$API/v1/metrics?window=5m" | jq '.channels.sms.sent')
[[ $sent -ge 150 ]] && pass "/v1/metrics reports $sent SMS sent" || fail "/v1/metrics sms.sent = $sent"
curl -sf "$API/metrics" | grep -q '^notifications_queue_depth' && pass "Prometheus queue depth exported" || fail "no queue depth metric"

echo "All smoke checks passed."
