#!/usr/bin/env bash
# Creates a fresh webhook.site URL that answers like the provider in the
# assessment (202 + {"messageId","status","timestamp"}), saves it as
# WEBHOOK_SITE_URL in .env and restarts the API and worker so the
# "webhook.site" provider uses it.
#
#   make webhook-url            # create and save; delivery stays on the current provider
#   make webhook-url SELECT=1   # ... and switch delivery to webhook.site
#
# Why: a free webhook.site URL accepts only 50 requests in total and
# expires after 7 days, but creating new ones is free. Needs curl and jq.
set -euo pipefail

cd "$(dirname "$0")/.."
API=${API:-http://localhost:8080}
SELECT=${SELECT:-0}
[[ ${1:-} == --select ]] && SELECT=1

command -v jq >/dev/null || { echo "jq is required (brew install jq)"; exit 1; }

# The response body is fixed: free URLs cannot generate a new messageId
# per request. The system only stores messageId, so that is fine.
response='{"messageId":"6f1c2a3e-0000-4000-8000-000000000000","status":"accepted","timestamp":"2026-01-01T00:00:00Z"}'
uuid=$(curl -sf -X POST https://webhook.site/token \
  -H 'Content-Type: application/json' -H 'Accept: application/json' \
  -d "$(jq -nc --arg body "$response" '{default_status: 202, default_content_type: "application/json", default_content: $body}')" |
  jq -r .uuid) || { echo "Could not create a webhook.site URL (offline or rate-limited?)"; exit 1; }
[[ -n $uuid && $uuid != null ]] || { echo "webhook.site returned no URL"; exit 1; }
url="https://webhook.site/$uuid"

# Save it in .env, replacing any previous WEBHOOK_SITE_URL line.
[[ -f .env ]] || cp .env.example .env
if grep -qE '^#? *WEBHOOK_SITE_URL=' .env; then
  tmp=$(mktemp)
  sed -E "s|^#? *WEBHOOK_SITE_URL=.*|WEBHOOK_SITE_URL=$url|" .env >"$tmp" && mv "$tmp" .env
else
  printf '\nWEBHOOK_SITE_URL=%s\n' "$url" >>.env
fi
echo "Saved WEBHOOK_SITE_URL in .env"

# Restart the services that read it, if the stack is running.
if [[ -n $(docker compose ps -q api 2>/dev/null) ]]; then
  echo "Restarting api and worker with the new URL..."
  docker compose up -d --wait api worker >/dev/null 2>&1
  if [[ $SELECT == 1 ]]; then
    curl -sf -X PUT "$API/v1/provider" -H 'Content-Type: application/json' -d '{"name":"webhook.site"}' >/dev/null
    echo "Delivery switched to webhook.site."
  else
    echo "Select \"webhook.site\" in the dashboard (http://localhost:3000) to send there."
  fi
else
  echo "Stack not running: start it with 'docker compose up -d'."
fi

cat <<EOF

  Watch requests arrive: https://webhook.site/#!/$uuid
  This URL accepts 50 requests (retries included) and expires in 7 days.
  Keep bursts and load tests on the mock provider.
EOF
