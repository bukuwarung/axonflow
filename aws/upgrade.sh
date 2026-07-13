#!/usr/bin/env bash
#
# upgrade.sh — one-command, idempotent upgrade of a running AxonFlow ECS
# Fargate stack via the mirror-to-your-ECR path.
#
# It does three things:
#   1. Mirrors the 6 partner images for <version> from GHCR into your ECR
#      (mirror-images-to-your-ecr.sh, manifest/multi-arch preserving).
#   2. Runs `aws cloudformation update-stack`, flipping ONLY the 6 image-tag
#      parameters to <version> and keeping every other parameter at its
#      current value (UsePreviousValue). By default it reuses the stack's
#      existing template (--use-previous-template); set TEMPLATE_URL to also
#      roll a new template.
#   3. Waits for UPDATE_COMPLETE, re-checks that every ECS service reached a
#      steady state on the new version, and prints a success / rollback summary.
#
# Configure via flags or aws/upgrade.env (see upgrade.env.example):
#   STACK_NAME     — the CloudFormation stack to upgrade   (required)
#   ECR_REGISTRY   — your ECR registry, e.g.
#                    123456789012.dkr.ecr.us-east-1.amazonaws.com/axonflow (required)
#   TEMPLATE_URL   — (optional) S3 URL of an updated template to roll
#   AXONFLOW_GHCR_USER / AXONFLOW_GHCR_TOKEN — GHCR pull creds (welcome bundle)
#
# Usage:
#   ./upgrade.sh <version>
#   STACK_NAME=my-stack ECR_REGISTRY=...amazonaws.com/axonflow ./upgrade.sh 9.9.0
#
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Load config file if present (flags/env override it via the ${VAR:-} pattern).
[ -f "$HERE/upgrade.env" ] && . "$HERE/upgrade.env"

VERSION_RAW="${1:?usage: upgrade.sh <version e.g. 9.9.0>}"
VERSION="v${VERSION_RAW#v}"

STACK_NAME="${STACK_NAME:?set STACK_NAME (flag/env/upgrade.env)}"
ECR_REGISTRY="${ECR_REGISTRY:?set ECR_REGISTRY (flag/env/upgrade.env)}"
TEMPLATE_URL="${TEMPLATE_URL:-}"

ECR_HOST="${ECR_REGISTRY%%/*}"
REGION="${REGION:-$(echo "$ECR_HOST" | sed -nE 's/^[0-9]+\.dkr\.ecr\.([a-z0-9-]+)\.amazonaws\.com$/\1/p')}"
: "${REGION:?could not derive REGION from ECR_REGISTRY; set REGION explicitly}"

# The 6 image-tag parameters flipped on upgrade.
TAG_PARAMS=(
  AgentImageTag
  OrchestratorImageTag
  CustomerPortalImageTag
  CustomerPortalUIImageTag
  PrometheusImageTag
  GrafanaImageTag
)

# Read the current stack parameters once (keys, and the registry value).
mapfile -t CUR_KEYS < <(aws cloudformation describe-stacks --region "$REGION" \
  --stack-name "$STACK_NAME" --query 'Stacks[0].Parameters[].ParameterKey' --output text | tr '\t' '\n')
if [ "${#CUR_KEYS[@]}" -eq 0 ]; then
  echo "FATAL: stack $STACK_NAME not found or has no parameters"; exit 1
fi
CUR_REGISTRY=$(aws cloudformation describe-stacks --region "$REGION" --stack-name "$STACK_NAME" \
  --query "Stacks[0].Parameters[?ParameterKey=='ContainerImageRegistry'].ParameterValue | [0]" \
  --output text 2>/dev/null || echo "")

echo "==> [1/3] Mirror $VERSION images from GHCR into your ECR"
if [ "$CUR_REGISTRY" = "$ECR_REGISTRY" ]; then
  "$HERE/mirror-images-to-your-ecr.sh" "$ECR_REGISTRY" "$VERSION"
else
  echo "  Stack pulls from '$CUR_REGISTRY', not the ECR target '$ECR_REGISTRY'."
  echo "  Skipping the mirror (it would not affect this stack's pulls). The image"
  echo "  tags will be flipped to $VERSION against the stack's current registry."
fi

echo ""
echo "==> [2/3] Updating stack $STACK_NAME (image tags -> $VERSION, all else unchanged)"

# Build the --parameters list: flip the 6 tags to $VERSION, UsePreviousValue on
# everything else. Robust to whatever param set the stack was created with.

is_tag_param() { local k="$1"; for t in "${TAG_PARAMS[@]}"; do [ "$k" = "$t" ] && return 0; done; return 1; }

PARAMS=()
for k in "${CUR_KEYS[@]}"; do
  if is_tag_param "$k"; then
    PARAMS+=("ParameterKey=$k,ParameterValue=$VERSION")
  else
    PARAMS+=("ParameterKey=$k,UsePreviousValue=true")
  fi
done

TEMPLATE_ARGS=(--use-previous-template)
[ -n "$TEMPLATE_URL" ] && TEMPLATE_ARGS=(--template-url "$TEMPLATE_URL")

# Capture stderr synchronously (no process-substitution race) so the
# "No updates are to be performed" idempotent case is detected reliably.
ERRFILE="$(mktemp)"
trap 'rm -f "$ERRFILE"' EXIT
if ! aws cloudformation update-stack --region "$REGION" --stack-name "$STACK_NAME" \
  "${TEMPLATE_ARGS[@]}" \
  --parameters "${PARAMS[@]}" \
  --capabilities CAPABILITY_IAM CAPABILITY_NAMED_IAM 2>"$ERRFILE"; then
  cat "$ERRFILE" >&2
  if grep -q "No updates are to be performed" "$ERRFILE"; then
    echo "Stack already at $VERSION — nothing to update. (idempotent no-op)"
    exit 0
  fi
  echo "FATAL: update-stack call failed"; exit 1
fi

echo "Waiting for UPDATE_COMPLETE (rolling deployment)..."
if aws cloudformation wait stack-update-complete --region "$REGION" --stack-name "$STACK_NAME"; then
  STATUS=UPDATE_COMPLETE
else
  STATUS=$(aws cloudformation describe-stacks --region "$REGION" --stack-name "$STACK_NAME" \
    --query 'Stacks[0].StackStatus' --output text 2>/dev/null || echo UNKNOWN)
fi

echo ""
echo "==> [3/3] Post-upgrade health"
CLUSTER=$(aws cloudformation describe-stack-resources --region "$REGION" --stack-name "$STACK_NAME" \
  --query 'StackResources[?ResourceType==`AWS::ECS::Cluster`].PhysicalResourceId' --output text 2>/dev/null || true)

svc_ok=1
if [ -n "$CLUSTER" ] && [ "$CLUSTER" != "None" ]; then
  for svc in $(aws ecs list-services --region "$REGION" --cluster "$CLUSTER" --query 'serviceArns[]' --output text 2>/dev/null); do
    read -r running desired <<<"$(aws ecs describe-services --region "$REGION" --cluster "$CLUSTER" --services "$svc" \
      --query 'services[0].[runningCount,desiredCount]' --output text)"
    name="${svc##*/}"
    if [ "$desired" = "0" ]; then
      echo "  SKIP $name  (disabled: 0 desired tasks)"
    elif [ "$running" = "$desired" ]; then
      echo "  OK   $name  ($running/$desired tasks running)"
    else
      echo "  WARN $name  ($running/$desired tasks running)"
      svc_ok=0
    fi
  done
fi

# Best-effort HTTP health check (only reachable if you run this from inside the
# VPC / over a tunnel for an internal ALB).
AGENT=$(aws cloudformation describe-stacks --region "$REGION" --stack-name "$STACK_NAME" \
  --query 'Stacks[0].Outputs[?OutputKey==`AgentEndpoint`].OutputValue' --output text 2>/dev/null || true)
if [ -n "$AGENT" ] && [ "$AGENT" != "None" ]; then
  echo "  Agent endpoint: $AGENT"
  if command -v curl >/dev/null 2>&1; then
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "${AGENT%/}/health" 2>/dev/null || echo "unreachable")
    echo "  GET ${AGENT%/}/health -> ${code} (internal ALB: reachable only from inside the VPC)"
  fi
fi

echo ""
if [ "$STATUS" = "UPDATE_COMPLETE" ] && [ "$svc_ok" = "1" ]; then
  echo "SUCCESS: $STACK_NAME upgraded to $VERSION; all ECS services steady."
  exit 0
else
  echo "ATTENTION: stack status=$STATUS. If CloudFormation rolled the update back,"
  echo "the previous version is still serving. Inspect events with:"
  echo "  aws cloudformation describe-stack-events --region $REGION --stack-name $STACK_NAME \\"
  echo "    --query 'StackEvents[?contains(ResourceStatus, \`FAILED\`)].[LogicalResourceId,ResourceStatusReason]' --output table"
  exit 1
fi
