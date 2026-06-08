#!/usr/bin/env bash
set -euo pipefail

# AxonFlow Enterprise — Install Script
# Validates configuration, pulls images, starts the platform, and verifies health.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="${SCRIPT_DIR}/.env"
COMPOSE_FILE="${SCRIPT_DIR}/docker-compose.yml"

# --------------------------------------------------------------------------
# Helpers
# --------------------------------------------------------------------------

log()   { printf '\033[1;34m[axonflow]\033[0m %s\n' "$*"; }
ok()    { printf '\033[1;32m[axonflow]\033[0m %s\n' "$*"; }
warn()  { printf '\033[1;33m[axonflow]\033[0m %s\n' "$*"; }
fail()  { printf '\033[1;31m[axonflow]\033[0m %s\n' "$*" >&2; exit 1; }

# --------------------------------------------------------------------------
# Pre-flight checks
# --------------------------------------------------------------------------

log "Checking prerequisites..."

if ! command -v docker &>/dev/null; then
  fail "Docker is not installed. Install Docker Engine 24+ and try again."
fi

if ! docker info &>/dev/null; then
  fail "Docker daemon is not running. Start Docker and try again."
fi

DOCKER_VERSION="$(docker version --format '{{.Server.Version}}' 2>/dev/null || echo "0")"
DOCKER_MAJOR="$(echo "$DOCKER_VERSION" | cut -d. -f1)"
if [ "${DOCKER_MAJOR:-0}" -lt 24 ] 2>/dev/null; then
  warn "Docker version $DOCKER_VERSION detected. Version 24+ is recommended."
fi

if ! docker compose version &>/dev/null; then
  fail "Docker Compose v2 is not available. Install Docker Compose v2+ and try again."
fi

if [ ! -f "$COMPOSE_FILE" ]; then
  fail "docker-compose.yml not found at $COMPOSE_FILE. Run this script from the axonflow-install directory."
fi

# --------------------------------------------------------------------------
# Load and validate .env
# --------------------------------------------------------------------------

if [ ! -f "$ENV_FILE" ]; then
  fail ".env file not found. Copy .env.example to .env and fill in your values:\n  cp .env.example .env"
fi

log "Loading configuration from .env..."
set -a
# shellcheck source=/dev/null
source "$ENV_FILE"
set +a

REQUIRED_VARS=(
  AXONFLOW_REGISTRY
  AGENT_DIGEST
  ORCHESTRATOR_DIGEST
  PORTAL_DIGEST
  PORTAL_UI_DIGEST
  PROMETHEUS_DIGEST
  GRAFANA_DIGEST
  AXONFLOW_VERSION
  AXONFLOW_DB_PASSWORD
  AXONFLOW_ORG_ID
  AXONFLOW_LICENSE_KEY
  AXONFLOW_INTERNAL_SERVICE_SECRET
  AXONFLOW_JWT_SECRET
  AXONFLOW_PORTAL_ADMIN_PASSWORD
  GRAFANA_ADMIN_PASSWORD
)

MISSING=()
for var in "${REQUIRED_VARS[@]}"; do
  val="${!var:-}"
  if [ -z "$val" ]; then
    MISSING+=("$var")
  fi
  # Catch unfilled placeholders
  if [[ "$val" == *"replace-with"* ]] || [[ "$val" == *"paste-from"* ]] || [[ "$val" == *"<"*">"* ]]; then
    MISSING+=("$var (still has placeholder value)")
  fi
done

if [ ${#MISSING[@]} -gt 0 ]; then
  fail "Missing or placeholder values in .env:\n$(printf '  - %s\n' "${MISSING[@]}")\nFill in the values from your welcome bundle and try again."
fi

ok "Configuration validated — all required variables set."

# --------------------------------------------------------------------------
# Registry authentication check
# --------------------------------------------------------------------------

log "Verifying registry authentication..."
TEST_IMAGE="${AXONFLOW_REGISTRY}/axonflow-agent@${AGENT_DIGEST}"
if ! docker manifest inspect "$TEST_IMAGE" &>/dev/null; then
  fail "Cannot access $AXONFLOW_REGISTRY. Check your registry login:\n\n  GHCR:  echo \"\$AXONFLOW_GHCR_TOKEN\" | docker login ghcr.io --username <username> --password-stdin\n  ECR:   aws ecr get-login-password --region us-east-1 | docker login --username AWS --password-stdin 686831565523.dkr.ecr.us-east-1.amazonaws.com"
fi

ok "Registry authentication verified."

# --------------------------------------------------------------------------
# Pull images
# --------------------------------------------------------------------------

log "Pulling platform images (this may take a few minutes on first run)..."
docker compose --env-file "$ENV_FILE" -f "$COMPOSE_FILE" pull

ok "All images pulled."

# --------------------------------------------------------------------------
# Start services
# --------------------------------------------------------------------------

log "Starting AxonFlow platform..."
docker compose --env-file "$ENV_FILE" -f "$COMPOSE_FILE" up -d

# --------------------------------------------------------------------------
# Wait for health
# --------------------------------------------------------------------------

log "Waiting for services to become healthy (up to 3 minutes)..."

MAX_WAIT=180
ELAPSED=0
INTERVAL=5

while [ "$ELAPSED" -lt "$MAX_WAIT" ]; do
  ALL_HEALTHY=true
  SERVICES=$(docker compose --env-file "$ENV_FILE" -f "$COMPOSE_FILE" ps --format '{{.Name}}' 2>/dev/null)
  for svc in $SERVICES; do
    STATUS=$(docker inspect --format='{{if .State.Health}}{{.State.Health.Status}}{{else}}no-healthcheck{{end}}' "$svc" 2>/dev/null || echo "unknown")
    if [ "$STATUS" != "healthy" ] && [ "$STATUS" != "no-healthcheck" ]; then
      ALL_HEALTHY=false
      break
    fi
  done

  if $ALL_HEALTHY; then
    break
  fi

  sleep "$INTERVAL"
  ELAPSED=$((ELAPSED + INTERVAL))
  log "Still waiting... (${ELAPSED}s / ${MAX_WAIT}s)"
done

if ! $ALL_HEALTHY; then
  warn "Some services did not become healthy within ${MAX_WAIT}s."
  warn "Run 'docker compose logs' to investigate."
  docker compose --env-file "$ENV_FILE" -f "$COMPOSE_FILE" ps
  exit 1
fi

ok "All services healthy."

# --------------------------------------------------------------------------
# Run verification
# --------------------------------------------------------------------------

log "Running health verification..."
"${SCRIPT_DIR}/verify.sh"

# --------------------------------------------------------------------------
# Done
# --------------------------------------------------------------------------

echo ""
ok "AxonFlow Enterprise is running."
echo ""
log "Quick reference:"
log "  Agent API:        http://localhost:8080"
log "  Orchestrator API: http://localhost:8081"
log "  Customer Portal:  http://localhost:3000  (org: ${AXONFLOW_ORG_ID} / your AXONFLOW_PORTAL_ADMIN_PASSWORD)"
log "  Grafana:          http://localhost:3001  (admin / your GRAFANA_ADMIN_PASSWORD)"
log "  Decision traces:  http://localhost:3001/d/decision-mode-traces"
echo ""
log "Next steps:"
log "  1. Open http://localhost:3000 and log in — org: ${AXONFLOW_ORG_ID}, password: your AXONFLOW_PORTAL_ADMIN_PASSWORD"
log "  2. Log in to Grafana at http://localhost:3001"
log "  3. Send a test request — see the setup runbook for details"
log "  4. Open the Decision Mode Traces dashboard to confirm spans land"
log "  Lost the portal password? Run ./reset-portal-credential.sh"
echo ""
log "To stop:  docker compose down"
log "To reset: docker compose down -v  (removes all data)"
