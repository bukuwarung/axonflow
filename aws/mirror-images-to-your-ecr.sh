#!/usr/bin/env bash
#
# mirror-images-to-your-ecr.sh — copy the 6 AxonFlow partner container images
# from the private AxonFlow GitHub Container Registry (GHCR) into an Amazon ECR
# registry in your own AWS account.
#
# Why mirror to your own ECR (the recommended production path):
#   - Same-region, in-VPC pulls are fast and can avoid public internet egress
#     entirely (via an ECR VPC endpoint) — useful for regulated / data-
#     sovereignty deployments.
#   - ECS pulls from ECR using the task execution role, whose credentials never
#     expire — no registry PAT to rotate and no PAT-expiry surprise mid-deploy.
#   - Fetch (mirror) is decoupled from rollout (deploy), so a slow pull never
#     blocks a stack update.
#
# The copy is manifest-preserving: it uses `docker buildx imagetools create`,
# which republishes the SAME multi-arch manifest list (linux/amd64 +
# linux/arm64) without pulling/repushing per-arch layers, so the content digest
# is carried over unchanged. This is idempotent — re-running is safe and lands
# the identical digests.
#
# Prerequisites:
#   - docker (with buildx) and the AWS CLI v2, both authenticated.
#   - A GHCR pull credential from your AxonFlow welcome bundle:
#       AXONFLOW_GHCR_USER  (your GHCR username)
#       AXONFLOW_GHCR_TOKEN (a read:packages token)
#   - AWS credentials for the account that owns <your-ecr-registry>, with
#     permission to create repositories and push images.
#
# Usage:
#   AXONFLOW_GHCR_USER=<user> AXONFLOW_GHCR_TOKEN=<token> \
#     ./mirror-images-to-your-ecr.sh <your-ecr-registry> [version]
#
# Example:
#   AXONFLOW_GHCR_USER=my-ghcr-user AXONFLOW_GHCR_TOKEN=ghp_xxx \
#     ./mirror-images-to-your-ecr.sh 123456789012.dkr.ecr.us-east-1.amazonaws.com/axonflow 9.9.0
#
#   -> mirrors ghcr.io/getaxonflow/<img>:v9.9.0 into
#      123456789012.dkr.ecr.us-east-1.amazonaws.com/axonflow/<img>:v9.9.0
#      for all 6 images, multi-arch preserved.
#
# Optional exact-bits pin: if a digest set file is present (one
# "<image>@sha256:<digest>" line per image, as shipped in your release bundle),
# export AXONFLOW_DIGEST_SET=/path/to/digest-set.txt and the source is pinned by
# digest instead of the moving tag.
#
set -euo pipefail

ECR_REGISTRY="${1:?usage: mirror-images-to-your-ecr.sh <your-ecr-registry> [version]}"
VERSION_RAW="${2:-9.9.0}"
# accept both "9.9.0" and "v9.9.0"; the published tags are v-prefixed
VERSION="v${VERSION_RAW#v}"

GHCR="ghcr.io/getaxonflow"
DIGEST_SET="${AXONFLOW_DIGEST_SET:-}"

# The 6 partner images. (performance-testing is internal-only and NOT mirrored.)
IMAGES=(
  axonflow-agent
  axonflow-orchestrator
  axonflow-customer-portal
  axonflow-customer-portal-ui
  axonflow-prometheus
  axonflow-grafana
)

# ECR registry looks like <account>.dkr.ecr.<region>.amazonaws.com[/<namespace>]
# The region is parsed from the host for standard partitions; override with
# ECR_REGION (or AWS_REGION) for other partitions/endpoints (China, FIPS, etc.).
ECR_HOST="${ECR_REGISTRY%%/*}"
ECR_REGION="${ECR_REGION:-${AWS_REGION:-$(echo "$ECR_HOST" | sed -nE 's/^[0-9]+\.dkr\.ecr\.([a-z0-9-]+)\.(amazonaws\.com|amazonaws\.com\.cn)$/\1/p')}}"
if [ -z "$ECR_REGION" ]; then
  echo "FATAL: could not parse region from ECR registry host '$ECR_HOST'"
  echo "       expected <account>.dkr.ecr.<region>.amazonaws.com[/<namespace>]"
  echo "       (set ECR_REGION explicitly for non-standard endpoints)"
  exit 1
fi
# repositories are created under the namespace path (everything after the host)
ECR_NAMESPACE="${ECR_REGISTRY#"$ECR_HOST"}"   # e.g. "/axonflow" or ""
ECR_NAMESPACE="${ECR_NAMESPACE#/}"            # strip leading slash

# --- helpers (never abort under set -e; empty output on error) --------------
# Space-separated os/arch list of a manifest ref, via a Go template so there is
# no python3 dependency. Attestation entries ("unknown/unknown") are harmless to
# the multi-arch grep below.
manifest_plats() {
  docker buildx imagetools inspect "$1" \
    --format '{{range .Manifest.Manifests}}{{.Platform.OS}}/{{.Platform.Architecture}} {{end}}' 2>/dev/null \
    || echo ""
}
manifest_digest() { docker buildx imagetools inspect "$1" --format '{{.Manifest.Digest}}' 2>/dev/null || echo ""; }
is_multiarch() { echo "$1" | grep -q 'linux/amd64' && echo "$1" | grep -q 'linux/arm64'; }

# source ref for an image: pinned digest (if a digest set is provided) else the tag
src_ref() {
  local img="$1"
  if [ -n "$DIGEST_SET" ] && [ -f "$DIGEST_SET" ]; then
    local dg
    dg="$(grep -E "(^|/)${img}@sha256:" "$DIGEST_SET" | head -1 | sed -E 's/.*@(sha256:[0-9a-f]+).*/\1/')"
    if [ -n "$dg" ]; then echo "${GHCR}/${img}@${dg}"; return; fi
  fi
  echo "${GHCR}/${img}:${VERSION}"
}

echo "== authenticating =="
: "${AXONFLOW_GHCR_USER:?set AXONFLOW_GHCR_USER (from your welcome bundle)}"
: "${AXONFLOW_GHCR_TOKEN:?set AXONFLOW_GHCR_TOKEN (from your welcome bundle)}"
echo "$AXONFLOW_GHCR_TOKEN" | docker login ghcr.io -u "$AXONFLOW_GHCR_USER" --password-stdin >/dev/null \
  || { echo "FATAL: GHCR docker login failed"; exit 1; }
aws ecr get-login-password --region "$ECR_REGION" \
  | docker login "$ECR_HOST" -u AWS --password-stdin >/dev/null \
  || { echo "FATAL: ECR docker login failed"; exit 1; }
echo "  GHCR login OK  ECR login OK  (region=$ECR_REGION)"

fail=0
for img in "${IMAGES[@]}"; do
  echo "== $img =="
  repo_name="${ECR_NAMESPACE:+$ECR_NAMESPACE/}$img"
  # ensure the destination ECR repo exists (idempotent)
  if ! aws ecr describe-repositories --region "$ECR_REGION" --repository-names "$repo_name" >/dev/null 2>&1; then
    aws ecr create-repository --region "$ECR_REGION" --repository-name "$repo_name" \
         --image-tag-mutability MUTABLE >/dev/null \
      || { echo "  ERROR: could not create ECR repo $repo_name"; fail=1; continue; }
  fi

  SRC="$(src_ref "$img")"
  DEST="${ECR_REGISTRY}/${img}:${VERSION}"
  echo "  $SRC -> $DEST"
  # manifest-preserving copy (multi-arch carried over, digest unchanged).
  # Guard so one failed image is reported and the rest still run (set -e would
  # otherwise abort on the first failure and hide later ones).
  docker buildx imagetools create -t "$DEST" "$SRC" \
    || { echo "  ERROR: mirror failed for $img ($SRC -> $DEST)"; fail=1; continue; }

  dest_plats="$(manifest_plats "$DEST")"
  dest_dg="$(manifest_digest "$DEST")"
  echo "  landed: ${dest_plats:-<none>}  @ ${dest_dg:-<none>}"
  is_multiarch "$dest_plats" \
    || { echo "  ERROR: $DEST is not multi-arch (need linux/amd64 + linux/arm64)"; fail=1; }
done

if [ "$fail" -ne 0 ]; then
  echo ""
  echo "ABORT: one or more images failed to mirror or the multi-arch gate."
  exit 1
fi

echo ""
echo "All 6 images mirrored to ${ECR_REGISTRY} at tag ${VERSION} (multi-arch preserved)."
echo "Set ContainerImageRegistry=${ECR_REGISTRY} and leave RegistryCredentialsSecretArn"
echo "empty in the CloudFormation stack — ECS pulls from ECR via the execution role."
