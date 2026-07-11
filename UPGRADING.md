# Upgrading AxonFlow

AxonFlow ships each new platform release as an updated **image digest set**
(delivered in your welcome bundle / 1Password) plus public **release notes**.
Upgrading is a digest swap and a restart — there is no reinstall, and your data
is preserved.

## Before you start

- Get the digest set for the target version — your AxonFlow contact shares it as
  a 1Password link — and read the
  [release notes](https://docs.getaxonflow.com/docs/releases) for that version.
- **Additive patch releases** (e.g. `9.2.1` → `9.2.2`) need no migrations or
  configuration changes. For **minor or major** releases, check the release
  notes for any migration or configuration steps before upgrading.

## Steps

1. **Update `.env`** with the new values from the digest set:
   - the six image digests — `AGENT_DIGEST`, `ORCHESTRATOR_DIGEST`,
     `PORTAL_DIGEST`, `PORTAL_UI_DIGEST`, `PROMETHEUS_DIGEST`, `GRAFANA_DIGEST`
   - `AXONFLOW_VERSION` (e.g. `9.2.2`)
   - leave `AXONFLOW_REGISTRY` unchanged.

2. **Pull the new images and restart:**
   ```bash
   docker compose pull
   docker compose up -d
   ```
   Your database, metrics, and Grafana configuration are preserved across the
   restart.

3. **Verify the upgrade:**
   ```bash
   ./verify.sh
   ```
   All services should report healthy. To confirm the running version and
   edition:
   ```bash
   curl -s http://localhost:8080/health
   ```
   `/health` should report the new version and `tier: Enterprise`.

4. **Update the Claude Code plugin (if you use it):** bump the AxonFlow
   marketplace `ref` to the new plugin tag (e.g. `v1.7.0`) in your Claude Code
   settings — or, for a pinned local clone,
   `git -C <plugin-dir> fetch && git checkout <tag>`. This is independent of the
   Docker deployment above — the plugin lives in your Claude Code settings, not
   in this bundle.

## Rollback

Re-pin the previous version's six digests and `AXONFLOW_VERSION` in `.env`, then
`docker compose up -d`. Images are content-addressed by digest, so the image
rollback is exact. This is safe for an **additive patch** release. A **minor or
major** release may run forward database migrations that are not automatically
reversed — contact support@getaxonflow.com before rolling one of those back.

## Version-specific notes

- **v9.8.1+ — per-user audit attribution:** to keep (or enable) per-user audit
  attribution from developer identity headers, set
  `AXONFLOW_TRUST_IDENTITY_HEADERS=true` in `.env` (agent-only; see the trust
  caveat in `.env.example`). The default is `false`: the agent ignores
  client-asserted identity headers and attributes audit rows to the fleet
  identity, logging a detection warning when a caller sends them. Set `true`
  only if your developers' identity comes from a trusted source (managed
  settings / MDM, the Claude Desktop proxy, or a gateway auth claim).

## Need help?

Email support@getaxonflow.com (24-hour response for evaluation partners), or see
the [README](README.md) for the full first-time install.
