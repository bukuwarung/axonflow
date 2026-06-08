# AxonFlow Enterprise — Self-Hosted Deployment

Deploy the AxonFlow Enterprise platform in your infrastructure with Docker Compose.

## Prerequisites

- Docker Engine 24+ with Docker Compose v2+
- Registry access (GHCR or ECR) — provided in your welcome bundle
- AxonFlow Enterprise license key — delivered via secure channel
- At least one LLM provider API key (OpenAI, Anthropic, or Google)

## Quickstart

### 1. Authenticate to the container registry

**GHCR** (recommended for GCP, Azure, on-prem, or mixed environments):

```bash
echo "$AXONFLOW_GHCR_TOKEN" \
  | docker login ghcr.io --username <username-from-welcome-bundle> --password-stdin
```

**ECR** (recommended for AWS-hosted deployments):

```bash
aws ecr get-login-password --region us-east-1 \
  | docker login --username AWS --password-stdin \
  686831565523.dkr.ecr.us-east-1.amazonaws.com
```

### 2. Configure

```bash
cp .env.example .env
```

Open `.env` and fill in the values from your welcome bundle:

- Uncomment your registry line (`AXONFLOW_REGISTRY`)
- Paste the 6 image digests matching your chosen registry
- Set your database and Grafana passwords
- Set your organization ID and license key
- Set `AXONFLOW_PORTAL_ADMIN_PASSWORD` — your Customer Portal login password
- Add at least one LLM provider API key

### 3. Install

```bash
chmod +x install.sh verify.sh reset-portal-credential.sh
./install.sh
```

The install script validates your configuration, pulls images, starts all services, and runs a health check. The full process takes 2-5 minutes depending on your network speed.

### 4. Verify

Once the install script completes, all services are running:

| Service | URL | Purpose |
|---------|-----|---------|
| Agent API | http://localhost:8080 | Policy enforcement gateway |
| Orchestrator API | http://localhost:8081 | Policy management and LLM routing |
| Customer Portal | http://localhost:3000 | Web dashboard for policies, audit trails, approvals |
| Grafana | http://localhost:3001 | Metrics dashboard (login: admin / your password) |

**Log into the Customer Portal** at http://localhost:3000 with:

- **Org:** your `AXONFLOW_ORG_ID`
- **Password:** your `AXONFLOW_PORTAL_ADMIN_PASSWORD`

This login is provisioned automatically on first boot — no manual setup. Once
you change the password (in the portal or with the recovery script below) the
new password sticks; the env var no longer overwrites it.

Lost the portal password? Reset it without losing data:

```bash
./reset-portal-credential.sh        # prompts for a new password
```

You can re-run the health check at any time:

```bash
./verify.sh
```

### 5. First policy test

> **⚠️ Requires the v8.5.1+ install bundle.** The dev-mode token endpoint below ships in the agent image for **v8.5.1 and later**. If `POST /api/v1/dev/token` returns `404` even in a `development` environment, your pinned `AGENT_DIGEST` predates v8.5.1 — use the v8.5.1+ digest set from your welcome bundle (or mint a `user_token` by hand, as described at the end of this section).

> **For this evaluation quickstart, set `AXONFLOW_ENVIRONMENT=development` in your `.env`** (uncomment the line in `.env.example`, then `docker compose up -d`). That enables the agent's dev-mode token endpoint `POST /api/v1/dev/token`, so you can mint an evaluation `user_token` directly from your Basic credential — no hand-built JWTs, no `generate-jwt.sh`, no setting `tenant_id` twice. The value is fail-closed: leave it unset/`production` and the endpoint **returns `404` by design** — production mints `user_token`s from your own IdP/app, not from AxonFlow. Don't rely on `/api/v1/dev/token` existing in production.

AxonFlow authenticates with **HTTP Basic** — `base64(AXONFLOW_ORG_ID:AXONFLOW_LICENSE_KEY)`. The full path is two calls:

```bash
source .env
AUTH=$(printf '%s:%s' "$AXONFLOW_ORG_ID" "$AXONFLOW_LICENSE_KEY" | base64 | tr -d '\n')

# 1. Mint an evaluation user_token from your Basic credential. Its tenant_id is
#    set automatically to your AXONFLOW_ORG_ID — a tenant mismatch is impossible.
USER_TOKEN=$(curl -s -X POST http://localhost:8080/api/v1/dev/token \
  -H "Authorization: Basic $AUTH" | jq -r .user_token)

# 2. Run a policy pre-check with that token and read the verdict.
curl -s -X POST http://localhost:8080/api/policy/pre-check \
  -H "Authorization: Basic $AUTH" \
  -H 'Content-Type: application/json' \
  -d "{\"user_token\":\"$USER_TOKEN\",\"client_id\":\"$AXONFLOW_ORG_ID\",\"query\":\"Summarize the quarterly report\"}" | jq .
```

The pre-check response includes the policy verdict (`"approved": true` / `false`), the matched policies, and a `context_id` you can look up in the **Customer Portal → Decisions / Audit** view.

You can also call **Decision Mode**, which needs no user token at all:

```bash
curl -s -X POST http://localhost:8080/api/v1/decide \
  -H "Authorization: Basic $AUTH" \
  -H 'Content-Type: application/json' \
  -d '{"stage":"llm","query":"Hello, world"}' | jq .
```

The Decision Mode response includes the verdict (`allow` / `deny` / `needs_approval`) and audit metadata.

> **What the dev-token endpoint does (and doesn't).** It mints a short-lived HS256 `user_token` signed with your `AXONFLOW_JWT_SECRET` and forces its `tenant_id` claim to equal your Basic-auth username — so the `403 Tenant mismatch` first-run error cannot happen on this path. It does **not** bypass license auth and **never** changes your `org_id`. It is registered **only** when `AXONFLOW_ENVIRONMENT` is an explicit non-production value (`development`, `dev`, `staging`, `local`); otherwise it returns `404`. To mint a token by hand instead (e.g. in production tooling), sign an HS256 JWT with `AXONFLOW_JWT_SECRET` whose `tenant_id` claim equals the Basic-auth username.

## Managing the Platform

```bash
# View logs
docker compose logs -f

# Stop the platform (data is preserved)
docker compose down

# Stop and remove all data (database, metrics, Grafana config)
docker compose down -v
```

## Troubleshooting

| Symptom | Fix |
|---------|-----|
| `401 Unauthorized` on image pull | Re-authenticate to your registry (GHCR PAT or ECR token) |
| `manifest unknown` on pull | Verify you are using the correct digest set for your registry |
| Agent fails with license error | Check `AXONFLOW_LICENSE_KEY` in `.env` matches your delivered key |
| Agent exits with database error | Wait for PostgreSQL to finish starting, then `docker compose restart axonflow-agent` |
| Portal shows blank page | Verify `axonflow-customer-portal` is healthy: `docker compose ps` |
| Grafana shows "No data" | Send a few test requests, wait 30 seconds, then refresh the dashboard |

## Documentation

- Platform documentation: https://docs.getaxonflow.com
- Google ADK integration: https://docs.getaxonflow.com/docs/integration/google-adk/
- n8n integration: https://docs.getaxonflow.com/docs/integration/n8n/
- LiteLLM integration: https://docs.getaxonflow.com/docs/integration/litellm/

## Support

- **Email:** support@getaxonflow.com
- **Direct:** saurabh.jain@getaxonflow.com
- **Response time:** 24 hours for evaluation partners

## License

The AxonFlow platform is licensed under the Business Source License 1.1 (BSL 1.1).
Your Enterprise evaluation license is delivered separately. See LICENSE.md for details.
