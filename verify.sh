#!/usr/bin/env bash
set -euo pipefail

# AxonFlow Enterprise — Health Verification
# Checks all platform service endpoints and reports pass/fail status.

ok()   { printf '  \033[1;32m✓\033[0m %s\n' "$*"; }
fail() { printf '  \033[1;31m✗\033[0m %s\n' "$*"; FAILURES=$((FAILURES + 1)); }
log()  { printf '\033[1;34m[axonflow]\033[0m %s\n' "$*"; }

FAILURES=0
MAX_RETRIES=3
RETRY_DELAY=5

check_endpoint() {
  local name="$1"
  local url="$2"
  local expected="${3:-200}"

  for attempt in $(seq 1 "$MAX_RETRIES"); do
    HTTP_CODE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "$url" 2>/dev/null || echo "000")
    if [ "$HTTP_CODE" = "$expected" ]; then
      ok "$name ($url) — HTTP $HTTP_CODE"
      return
    fi
    if [ "$attempt" -lt "$MAX_RETRIES" ]; then
      sleep "$RETRY_DELAY"
    fi
  done

  fail "$name ($url) — expected HTTP $expected, got $HTTP_CODE"
}

echo ""
log "Verifying AxonFlow platform health..."
echo ""

check_endpoint "Agent"            "http://localhost:8080/health"
check_endpoint "Orchestrator"     "http://localhost:8081/health"
check_endpoint "Portal API"       "http://localhost:8082/health"
check_endpoint "Portal UI"        "http://localhost:3000/"        "307"
check_endpoint "Prometheus"       "http://localhost:9090/prometheus/-/healthy"
check_endpoint "Grafana"          "http://localhost:3001/api/health"

echo ""
if [ "$FAILURES" -eq 0 ]; then
  log "All 6 endpoints healthy."
  exit 0
else
  log "$FAILURES endpoint(s) failed. Check 'docker compose logs' for details."
  exit 1
fi
