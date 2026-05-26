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
- Add at least one LLM provider API key

### 3. Install

```bash
chmod +x install.sh verify.sh
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

You can re-run the health check at any time:

```bash
./verify.sh
```

### 5. First policy test

Obtain an authentication token and send a test request through the gateway:

```bash
TOKEN=$(curl -s http://localhost:8080/v1/auth/token \
  -H 'Content-Type: application/json' \
  -d '{"client_id":"YOUR_ORG_ID","client_secret":"YOUR_LICENSE_KEY"}' | jq -r .token)

curl -s -X POST http://localhost:8080/v1/gateway \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -d '{
    "model": "gpt-4o",
    "messages": [{"role": "user", "content": "Hello, world"}]
  }' | jq .
```

The response includes policy decisions and audit metadata.

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
