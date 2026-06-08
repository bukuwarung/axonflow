#!/usr/bin/env bash
#
# reset-portal-credential.sh — reset the deployment org's Customer Portal login
# password (#2552).
#
# Use this when the portal login credential for the deployment org is lost. In
# enterprise / in-vpc deployments the portal login identity is the deployment
# org (ORG_ID); its password is bootstrapped at first boot from
# AXONFLOW_PORTAL_ADMIN_PASSWORD, but once an operator changes it (in the portal
# UI, or with this script) the bootstrap env var no longer overwrites it. This
# script sets a fresh password_hash directly in the database, idempotently.
#
# Mechanism: it hashes the new password with bcrypt INSIDE Postgres via
# pgcrypto's crypt(pw, gen_salt('bf', 10)). pgcrypto emits a standard `$2a$`
# bcrypt hash that the portal's Go login path (bcrypt.CompareHashAndPassword)
# verifies natively — so the reset credential authenticates immediately, with no
# portal restart and no dependency on the portal being up. Re-running it is safe
# (each run writes a fresh hash for the same password).
#
# Connection (first match wins):
#   1. RESET_PSQL_CMD   — explicit psql invocation. The SQL is fed on stdin, so a
#                         `docker exec` override MUST include -i, e.g.
#                         RESET_PSQL_CMD="docker exec -i my-pg psql -U axonflow -d axonflow"
#   2. DATABASE_URL     — a libpq URL: psql "$DATABASE_URL"
#   3. docker compose   — when run from a directory with a docker-compose.yml that
#                         defines a `postgres` service (the axonflow-install bundle):
#                         docker compose exec -T postgres psql -U <user> -d <db>
#   4. psql + PG* env   — falls back to a bare `psql` using PGHOST/PGUSER/... (or
#                         the DB_* overrides below).
#
# Usage:
#   ./reset-portal-credential.sh [--org ORG_ID] [NEW_PASSWORD]
#
#   # Bundle (run from the axonflow-install dir; org read from .env):
#   ./reset-portal-credential.sh 'my-new-strong-password'
#
#   # Prompt for the password instead of passing it on the command line:
#   ./reset-portal-credential.sh --org acme
#
#   # Against an RDS / external DB:
#   DATABASE_URL="postgres://axonflow:pw@db:5432/axonflow" \
#       ./reset-portal-credential.sh --org acme 'my-new-strong-password'
#
# Org resolution (first non-empty): --org flag → AXONFLOW_ORG_ID → ORG_ID →
# AXONFLOW_ORG_ID/ORG_ID from a ./.env in the working directory.
#
# Exit 0 = the password_hash was updated and the org row exists.

set -euo pipefail

MIN_LEN=8
DB_USER="${DB_USER:-axonflow}"
DB_NAME="${DB_NAME:-axonflow}"
COMPOSE_FILE="${COMPOSE_FILE:-docker-compose.yml}"

log()  { printf '\033[1;34m[reset-portal]\033[0m %s\n' "$*"; }
ok()   { printf '\033[1;32m[reset-portal]\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31m[reset-portal]\033[0m %s\n' "$*" >&2; exit 1; }

# --------------------------------------------------------------------------
# Parse args
# --------------------------------------------------------------------------
ORG=""
NEWPW=""
while [ $# -gt 0 ]; do
  case "$1" in
    --org) ORG="${2:-}"; shift 2 ;;
    --org=*) ORG="${1#--org=}"; shift ;;
    -h|--help) sed -n '2,46p' "$0"; exit 0 ;;
    *) NEWPW="$1"; shift ;;
  esac
done

# --------------------------------------------------------------------------
# Resolve org id
# --------------------------------------------------------------------------
if [ -z "$ORG" ]; then ORG="${AXONFLOW_ORG_ID:-}"; fi
if [ -z "$ORG" ]; then ORG="${ORG_ID:-}"; fi
if [ -z "$ORG" ] && [ -f .env ]; then
  ORG="$(grep -E '^(AXONFLOW_ORG_ID|ORG_ID)=' .env | head -1 | cut -d= -f2- | tr -d '"' )"
fi
[ -n "$ORG" ] || fail "Could not resolve the org id. Pass --org <ORG_ID>, or set AXONFLOW_ORG_ID, or run from a directory with a .env that defines AXONFLOW_ORG_ID."

# --------------------------------------------------------------------------
# Resolve / prompt for the new password
# --------------------------------------------------------------------------
if [ -z "$NEWPW" ]; then
  printf 'New portal password for org %s: ' "$ORG" >&2
  read -rs NEWPW; echo >&2
  printf 'Confirm: ' >&2
  read -rs NEWPW_CONFIRM; echo >&2
  [ "$NEWPW" = "$NEWPW_CONFIRM" ] || fail "Passwords do not match."
fi
[ -n "$NEWPW" ] || fail "New password must not be empty."
[ "${#NEWPW}" -ge "$MIN_LEN" ] || fail "New password must be at least ${MIN_LEN} characters."

# --------------------------------------------------------------------------
# Build the psql command
# --------------------------------------------------------------------------
PSQL=()
if [ -n "${RESET_PSQL_CMD:-}" ]; then
  # shellcheck disable=SC2206
  PSQL=(${RESET_PSQL_CMD})
  log "Using RESET_PSQL_CMD."
elif [ -n "${DATABASE_URL:-}" ]; then
  PSQL=(psql "$DATABASE_URL")
  log "Using DATABASE_URL."
elif command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1 \
     && [ -f "$COMPOSE_FILE" ] && [ -n "$(docker compose -f "$COMPOSE_FILE" ps -q postgres 2>/dev/null)" ]; then
  PSQL=(docker compose -f "$COMPOSE_FILE" exec -T postgres psql -U "$DB_USER" -d "$DB_NAME")
  log "Using the bundled docker compose 'postgres' service."
elif command -v psql >/dev/null 2>&1; then
  PSQL=(psql -U "$DB_USER" -d "$DB_NAME")
  log "Using psql with PG* environment defaults."
else
  fail "No way to reach Postgres. Set RESET_PSQL_CMD or DATABASE_URL, or run from the axonflow-install dir with the stack up, or install the psql client."
fi

# --------------------------------------------------------------------------
# Reset — pgcrypto bcrypt UPDATE. psql ':'var'' interpolation safely quotes the
# password + org as SQL string literals (no injection through either value).
# --------------------------------------------------------------------------
log "Resetting portal password for org '$ORG'..."

OUT="$(
  "${PSQL[@]}" -v ON_ERROR_STOP=1 -X -q -t -A \
    -v pw="$NEWPW" -v org="$ORG" <<'SQL'
CREATE EXTENSION IF NOT EXISTS pgcrypto;
-- Scope the org for FORCE RLS: harmless when the connecting role bypasses RLS
-- (table owner / superuser — the bundled postgres + AXONFLOW_DB_USE_APP_ROLE=false
-- path), load-bearing when it doesn't (an axonflow_app_role / NOBYPASSRLS DSN),
-- so the UPDATE matches the org's own row instead of silently affecting 0 rows.
SELECT set_config('app.current_org_id', :'org', true);
WITH upd AS (
  UPDATE organizations
     SET password_hash = crypt(:'pw', gen_salt('bf', 10)),
         updated_at    = CURRENT_TIMESTAMP
   WHERE org_id = :'org'
  RETURNING 1
)
SELECT count(*) FROM upd;
SQL
)" || fail "psql failed. Check the connection (RESET_PSQL_CMD / DATABASE_URL / docker compose) and that pgcrypto can be created by this role."

# The SQL prints two result rows (set_config's value, then the UPDATE count);
# the count is the LAST non-blank line.
ROWS="$(printf '%s\n' "$OUT" | grep -v '^[[:space:]]*$' | tail -1 | tr -d '[:space:]')"
if [ "$ROWS" = "1" ]; then
  ok "Portal password reset for org '$ORG'."
  log "Log in at the portal with org '$ORG' and the new password."
elif [ "$ROWS" = "0" ]; then
  fail "No organization row found for org_id '$ORG'. Has the agent booted and registered it yet? (The agent creates/promotes the deployment org row at boot.)"
else
  fail "Unexpected result from the reset query: '$OUT'"
fi
