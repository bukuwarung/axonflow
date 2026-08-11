# agentgateway-adapter — custom Envoy ext_authz v3 PEP for AxonFlow

Buildable counterpart to the Enterprise `axonflow-gateway-adapters` binary
described in docs.getaxonflow.com/docs/integration/agentgateway. Provided for
deployments that need the gateway ⇄ PDP gRPC seam but do not have access to
the closed-source Enterprise build (AID-100).

## What it does

Terminates Envoy `ext_authz` v3 gRPC on `:9090` (agentgateway's `extAuthz`
policy calls it via `envoyproxy/go-control-plane`), asks the AxonFlow
Decision API (`POST /api/v1/decide`) for a verdict on every request, and
maps the answer back to `ext_authz`:

| PDP verdict | gRPC response | HTTP surface (via denied body) |
|---|---|---|
| `allow` | `OK`, stamps `x-axonflow-decision-id` + `x-axonflow-trace-id` on the upstream request | forwarded |
| `deny` | `PermissionDenied` | 403 with structured JSON `{error:{message, decision_id, trace_id, type:"policy_violation"}}` |
| `needs_approval` | `PermissionDenied` | 403 (treated as deny for now — see TODO) |
| PDP transport / 5xx, `AXONFLOW_FAIL_MODE=closed` | `Unavailable` | 503 |
| PDP transport / 5xx, `AXONFLOW_FAIL_MODE=open` | `OK` (traffic forwarded) | — |
| PDP 4xx (bad creds, rate limit) | `PermissionDenied` | 502 — never fail-open eligible |

Also runs `grpc.health.v1.Health` on the same listener so docker-compose
healthchecks and agentgateway probes work.

## Scope

**Implemented:** Envoy `ext_authz` v3 (request-plane authorisation) — enough
for agentgateway's LLM plane, where response-plane body rewriting would
break SSE streaming.

**Deliberately out of scope in this build:**

- **`ext_proc` v3** (response-plane body governance + fulfilment of
  request-phase `redact_pii` obligations via `/api/v1/mcp/check-output`).
  `ext_authz` cannot rewrite bodies; if the PDP returns a
  request-phase `redact_pii` obligation, this adapter **fails closed**
  rather than forward unredacted content (ADR-056 discipline).
- **agentgateway's proprietary `mcpGuardrails` proto.** That surface is
  documented on docs.getaxonflow.com but its proto is not published in this
  repo. Configure the MCP plane's guardrails via
  `mcp.policies.mcpGuardrails.processors[].kind: remote` against a future
  build of this adapter that carries the proto, or route MCP-plane
  governance through the PDP's `/api/v1/mcp/check-input|check-output`
  endpoints directly.
- Vendor-supplied Enterprise binary: swap this image for the Enterprise
  build whenever it becomes available — the env-var contract matches.

## Build

```
# from this directory
go mod tidy
go build ./cmd/axonflow-gateway-adapters   # local binary
docker build -t axonflow-gateway-adapters:local .   # container
```

## Environment

| Var | Default | Purpose |
|---|---|---|
| `GATEWAY_ADAPTERS_LISTEN` | `:9090` | gRPC bind address. |
| `AXONFLOW_ENDPOINT` | `http://localhost:8080` | PDP base URL. Use the AxonFlow agent's ALB URL in staging/prod. |
| `AXONFLOW_ORG_ID` | — | Enterprise org (HTTP Basic username on decide calls). |
| `AXONFLOW_LICENSE_KEY` | — | Enterprise license (HTTP Basic password). Omit both for Community-mode anonymous calls. |
| `AXONFLOW_TENANT_ID` | — | Stamped on every decide request's `caller_identity`. |
| `AXONFLOW_GATEWAY_ID` | `agentgateway` | Identifies this PEP in audit rows. |
| `AXONFLOW_STAGE` | `llm` | Decision stage — `llm`, `tool`, or `agent`. Set to `tool` on adapters fronting an MCP plane. |
| `AXONFLOW_FAIL_MODE` | `closed` | Behaviour on transport / 5xx PDP errors. `closed` blocks; `open` forwards. 4xx always blocks. |
| `AXONFLOW_REQUEST_TIMEOUT` | `10s` | PDP call timeout (Go `time.ParseDuration`). |
| `AXONFLOW_MAX_BODY_BYTES` | `8388608` | Cap on request-body bytes forwarded to the PDP. |

## Wire this to agentgateway

In `config.yaml` under `llm.policies` (v1.3.1 schema):

```yaml
extAuthz:
  host: axonflow-adapters:9090
  protocol:
    grpc: {}
  failureMode: allow      # rollout — flip to deny in Phase 4
  includeRequestBody:
    maxRequestBytes: 65536
```

For the MCP plane, use `mcp.policies.mcpGuardrails` with `kind: remote`
pointing at the same host — pending the mcpGuardrails-proto follow-up
noted above.
