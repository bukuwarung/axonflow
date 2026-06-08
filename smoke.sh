#!/usr/bin/env bash
set -uo pipefail

# AxonFlow Enterprise — end-to-end auth smoke (#2542).
#
# Walks the FULL "first policy test" path against a running install stack and
# fails red if any step breaks — so a stale README example cannot ship silently:
#
#   1. Agent is healthy.
#   2. POST /api/v1/dev/token mints a user_token from the Basic credential, with
#      tenant_id forced to the org id (requires AXONFLOW_ENVIRONMENT=development).
#   3. POST /api/policy/pre-check with that token returns a verdict.
#   4. POST /api/v1/decide records a decision.
#   5. The decision is visible via GET /api/v1/decisions (the data the Customer
#      Portal "Decisions" view renders).
#   6. Portal login: POST /api/v1/auth/login with ORG_ID + AXONFLOW_PORTAL_ADMIN_PASSWORD
#      returns 200 + a session — the deployment-org credential auto-provisioned
#      at first boot (#2552), so the portal is loginable out-of-box.
#   7. Fail-closed gate: an agent booted with ENVIRONMENT=production returns 404
#      for /api/v1/dev/token — so the smoke documents the gate too.
#
# Run it from the install directory after `./install.sh`. Requires: docker, jq,
# curl. Reads org/license/jwt/portal-password from `.env`.
#
# Env overrides (defaults target the bundled stack):
#   AGENT_URL   (http://localhost:8080)   ORCH_URL (http://localhost:8081)
#   PORTAL_URL  (http://localhost:8082)   SMOKE_DB_HOST (postgres)
#   AGENT_CONTAINER (auto-detected)
#   SKIP_PROD_CHECK=1  → skip step 7 (NOT recommended; it proves fail-closed)

ok()   { printf '  \033[1;32m✓\033[0m %s\n' "$*"; }
bad()  { printf '  \033[1;31m✗\033[0m %s\n' "$*"; FAILURES=$((FAILURES + 1)); }
log()  { printf '\033[1;34m[smoke]\033[0m %s\n' "$*"; }
FAILURES=0

AGENT_URL="${AGENT_URL:-http://localhost:8080}"
ORCH_URL="${ORCH_URL:-http://localhost:8081}"
PORTAL_URL="${PORTAL_URL:-http://localhost:8082}"
SMOKE_DB_HOST="${SMOKE_DB_HOST:-postgres}"

# --- load credentials from .env -------------------------------------------
if [ -f .env ]; then set -a; . ./.env; set +a; fi
ORG="${AXONFLOW_ORG_ID:-}"
LIC="${AXONFLOW_LICENSE_KEY:-}"
if [ -z "$ORG" ] || [ -z "$LIC" ]; then
  bad "AXONFLOW_ORG_ID / AXONFLOW_LICENSE_KEY not set (source .env first)"; exit 1
fi
AUTH=$(printf '%s:%s' "$ORG" "$LIC" | base64 | tr -d '\n')
WORK="$(mktemp -d)"; trap 'rm -rf "$WORK"' EXIT

echo ""; log "AxonFlow end-to-end auth smoke"
echo "  agent=$AGENT_URL  orchestrator=$ORCH_URL  org=$ORG"
echo ""

# --- 1. agent health -------------------------------------------------------
if [ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "$AGENT_URL/health")" = "200" ]; then
  ok "agent healthy"
else
  bad "agent not healthy at $AGENT_URL — is the stack up?"; exit 1
fi

# --- 2. mint a dev token ---------------------------------------------------
code=$(curl -s -o "$WORK/mint.json" -w '%{http_code}' -X POST "$AGENT_URL/api/v1/dev/token" -H "Authorization: Basic $AUTH")
USER_TOKEN=$(jq -r '.user_token // empty' < "$WORK/mint.json" 2>/dev/null)
MTEN=$(jq -r '.tenant_id // empty' < "$WORK/mint.json" 2>/dev/null)
if [ "$code" = "200" ] && [ -n "$USER_TOKEN" ] && [ "$MTEN" = "$ORG" ]; then
  ok "minted user_token (tenant_id=$MTEN, forced to the org)"
elif [ "$code" = "404" ]; then
  bad "/api/v1/dev/token → 404. Set AXONFLOW_ENVIRONMENT=development in .env and 'docker compose up -d' to enable it. If it stays 404 in development, your pinned AGENT_DIGEST predates the dev-token endpoint."; exit 1
else
  bad "/api/v1/dev/token → HTTP $code (expected 200)"; sed 's/^/      /' "$WORK/mint.json"; exit 1
fi

# --- 3. pre-check returns a verdict ---------------------------------------
code=$(curl -s -o "$WORK/pc.json" -w '%{http_code}' -X POST "$AGENT_URL/api/policy/pre-check" \
  -H "Authorization: Basic $AUTH" -H 'Content-Type: application/json' \
  -d "{\"user_token\":\"$USER_TOKEN\",\"client_id\":\"$ORG\",\"query\":\"Summarize the quarterly report\"}")
if [ "$code" = "200" ] && jq -e 'has("approved")' < "$WORK/pc.json" >/dev/null 2>&1; then
  ok "pre-check verdict: approved=$(jq -r .approved < "$WORK/pc.json") (context_id=$(jq -r .context_id < "$WORK/pc.json" | cut -c1-8)…)"
else
  bad "pre-check → HTTP $code without a verdict"; sed 's/^/      /' "$WORK/pc.json"
fi

# --- 4. decide records a decision -----------------------------------------
code=$(curl -s -o "$WORK/dec.json" -w '%{http_code}' -X POST "$AGENT_URL/api/v1/decide" \
  -H "Authorization: Basic $AUTH" -H 'Content-Type: application/json' \
  -d '{"stage":"llm","query":"Hello, world"}')
DID=$(jq -r '.decision_id // empty' < "$WORK/dec.json" 2>/dev/null)
if [ "$code" = "200" ] && [ -n "$DID" ]; then
  ok "decision recorded: verdict=$(jq -r .verdict < "$WORK/dec.json") decision_id=${DID:0:8}…"
else
  bad "/api/v1/decide → HTTP $code without a decision_id"; sed 's/^/      /' "$WORK/dec.json"
fi

# --- 5. the decision is visible (Portal Decisions view data source) -------
if [ -n "${DID:-}" ]; then
  found=""
  for _ in $(seq 1 10); do
    echo '{}' > "$WORK/list.json"
    curl -s "$ORCH_URL/api/v1/decisions?limit=20" -H "X-Tenant-ID: $ORG" -o "$WORK/list.json" 2>/dev/null || true
    if jq -e --arg d "$DID" '.decisions[]? | select(.decision_id==$d)' < "$WORK/list.json" >/dev/null 2>&1; then found=1; break; fi
    sleep 1
  done
  if [ -n "$found" ]; then
    ok "decision ${DID:0:8}… visible via GET /api/v1/decisions (Portal → Decisions)"
  else
    bad "decision ${DID:0:8}… not visible via $ORCH_URL/api/v1/decisions"; sed 's/^/      /' "$WORK/list.json" 2>/dev/null || true
  fi
fi

# --- 6. portal login with the auto-provisioned deployment-org credential ---
# #2552: an enterprise/in-vpc install bootstraps the portal password for ORG_ID
# from AXONFLOW_PORTAL_ADMIN_PASSWORD at first boot — so an out-of-box login must
# succeed with no manual SQL. This is the data the Customer Portal sign-in uses.
PORTAL_PW="${AXONFLOW_PORTAL_ADMIN_PASSWORD:-}"
if [ -z "$PORTAL_PW" ]; then
  bad "AXONFLOW_PORTAL_ADMIN_PASSWORD not set (source .env) — cannot verify the auto-provisioned portal login"
else
  lcode=$(curl -s -o "$WORK/login.json" -w '%{http_code}' -X POST "$PORTAL_URL/api/v1/auth/login" \
    -H 'Content-Type: application/json' \
    -d "{\"org_id\":\"$ORG\",\"password\":$(printf '%s' "$PORTAL_PW" | jq -Rs .)}")
  SID=$(jq -r '.session_id // empty' < "$WORK/login.json" 2>/dev/null)
  if [ "$lcode" = "200" ] && [ -n "$SID" ]; then
    ok "portal login (org=$ORG) → 200 + session (auto-provisioned credential, #2552)"
  else
    bad "portal login (org=$ORG) → HTTP $lcode without a session_id (expected 200). The deployment-org credential should be auto-provisioned at first boot."; sed 's/^/      /' "$WORK/login.json" 2>/dev/null || true
  fi
fi

# --- 7. fail-closed gate: an ENVIRONMENT=production agent returns 404 ------
if [ "${SKIP_PROD_CHECK:-0}" = "1" ]; then
  log "step 7 (prod-404) skipped via SKIP_PROD_CHECK=1"
else
  CONTAINER="${AGENT_CONTAINER:-$(docker ps --filter 'name=axonflow-agent' --format '{{.Names}}' 2>/dev/null | head -1)}"
  if [ -z "$CONTAINER" ] || ! command -v docker >/dev/null 2>&1; then
    bad "cannot prove the prod-404 gate: no running axonflow-agent container found (set AGENT_CONTAINER, or SKIP_PROD_CHECK=1 to bypass)"
  else
    IMG=$(docker inspect --format '{{.Config.Image}}' "$CONTAINER" 2>/dev/null)
    NET=$(docker inspect --format '{{range $k,$_ := .NetworkSettings.Networks}}{{$k}}{{"\n"}}{{end}}' "$CONTAINER" 2>/dev/null | head -1)
    EPH="axonflow-devtoken-prodcheck-$$"
   if [ -z "$IMG" ] || [ -z "$NET" ]; then
    bad "could not inspect agent container '$CONTAINER' (image/network not found) — set AGENT_CONTAINER to a running agent, or SKIP_PROD_CHECK=1"
   else
    docker rm -f "$EPH" >/dev/null 2>&1 || true
    # Boot an ephemeral agent in the bundle's PRODUCTION posture (ENVIRONMENT
    # AND DEPLOYMENT_KIND both production — the gate opens on ANY explicit
    # non-prod signal, so both must be pinned), sharing the stack network + DB.
    # Let Docker pick the host port to avoid collisions; surface boot failures.
    if ! run_err=$(docker run -d --rm --name "$EPH" --network "$NET" -p 127.0.0.1::8080 \
      -e ENVIRONMENT=production -e DEPLOYMENT_KIND=production -e DEPLOYMENT_MODE=in-vpc-enterprise \
      -e AXONFLOW_DB_USE_APP_ROLE=false -e PORT=8080 \
      -e ORG_ID="$ORG" -e AXONFLOW_LICENSE_KEY="$LIC" \
      -e AXONFLOW_JWT_SECRET="${AXONFLOW_JWT_SECRET:-}" -e JWT_SECRET="${AXONFLOW_JWT_SECRET:-}" \
      -e AXONFLOW_INTERNAL_SERVICE_SECRET="${AXONFLOW_INTERNAL_SERVICE_SECRET:-}" \
      -e DATABASE_HOST="$SMOKE_DB_HOST" -e DATABASE_PORT=5432 -e DATABASE_USER=axonflow \
      -e DATABASE_PASSWORD="${AXONFLOW_DB_PASSWORD:-axonflow}" -e DATABASE_NAME=axonflow -e DATABASE_SSLMODE=disable \
      "$IMG" 2>&1); then
      bad "could not boot the ephemeral prod-check agent: $run_err"
    else
      HOSTPORT=$(docker port "$EPH" 8080 2>/dev/null | head -1 | sed 's/.*://')
      pcode="000"
      if [ -z "$HOSTPORT" ]; then
        bad "ephemeral prod-check agent has no mapped port (docker port failed)"
      else
        for _ in $(seq 1 60); do
          [ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "http://127.0.0.1:${HOSTPORT}/health" 2>/dev/null)" = "200" ] || { sleep 1; continue; }
          pcode=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -X POST "http://127.0.0.1:${HOSTPORT}/api/v1/dev/token" -H "Authorization: Basic $AUTH")
          break
        done
        # A never-healthy container leaves pcode=000 → not 404 → fails (closed).
        if [ "$pcode" = "404" ]; then
          ok "fail-closed gate: ENVIRONMENT=production agent → /api/v1/dev/token 404"
        else
          bad "prod-mode agent returned HTTP $pcode for /api/v1/dev/token (expected 404 — the minter must be absent in production)"
        fi
      fi
    fi
    docker rm -f "$EPH" >/dev/null 2>&1 || true
   fi
  fi
fi

echo ""
if [ "$FAILURES" -eq 0 ]; then
  log "PASS — full eval auth path works and the dev-token gate is fail-closed in production."
  exit 0
else
  log "FAIL — $FAILURES check(s) failed."
  exit 1
fi
