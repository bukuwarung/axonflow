# Deploy AxonFlow on AWS ECS Fargate (CloudFormation)

This directory contains everything needed to deploy the AxonFlow Enterprise
platform into **your own AWS account** as an ECS Fargate stack, using
CloudFormation. It provisions the agent and orchestrator, an optional customer
portal and portal UI, optional Prometheus/Grafana monitoring, an RDS PostgreSQL
database, an Application Load Balancer, and an EFS volume for the audit trail.

The container images are license-gated and are pulled from the private AxonFlow
GitHub Container Registry (GHCR) using the credentials in your welcome bundle.

| File | Purpose |
|------|---------|
| `cloudformation-ecs-fargate.yaml` | The CloudFormation template. |
| `mirror-images-to-your-ecr.sh` | Copy the 6 images from GHCR into your ECR (recommended for production). |
| `upgrade.sh` | One-command upgrade of a running stack via the mirror-to-ECR path. |
| `upgrade.env.example` | Config template for `upgrade.sh` / the mirror script. |

---

## Prerequisites

- **A VPC** with:
  - **2 public subnets** in 2 Availability Zones (for the load balancer), each
    routing `0.0.0.0/0` to an Internet Gateway.
  - **2 private subnets** in the same 2 AZs (for the app services and RDS).
- **AWS CLI v2** and **Docker (with buildx)**, both authenticated to your
  account.
- **GHCR pull credentials** from your welcome bundle:
  `AXONFLOW_GHCR_USER` (username) and `AXONFLOW_GHCR_TOKEN` (a `read:packages`
  token).
- *(Optional)* an **ACM certificate** ARN (or a domain name to auto-issue one)
  for HTTPS on the load balancer. Without it the ALB serves HTTP only.

---

## Choose a registry option

You point the stack at a container registry with the `ContainerImageRegistry`
parameter. There are two supported ways to get the images to a registry ECS can
pull from.

### Option 1 — GHCR-direct (quickstart / evaluation)

Fewest moving parts: ECS pulls the images straight from the private GHCR. You
give the stack a Secrets Manager secret holding your GHCR pull credential.

1. Create the pull secret. **The value must be JSON of exactly this shape** — a
   bare token string will not work:

   ```json
   {"username":"<your-ghcr-username>","password":"<your-read:packages-token>"}
   ```

   ```bash
   aws secretsmanager create-secret \
     --name axonflow/ghcr-pull \
     --secret-string '{"username":"YOUR_GHCR_USER","password":"YOUR_GHCR_TOKEN"}' \
     --query ARN --output text
   ```

2. Deploy with:
   - `ContainerImageRegistry = ghcr.io/getaxonflow` (the default)
   - `RegistryCredentialsSecretArn = <the ARN printed above>`

That is all — see [Deploy](#deploy). The task execution role is granted read
access to that one secret so ECS can authenticate the pull.

> A `read:packages` token expires on GitHub's schedule. If it expires, image
> pulls start failing with `CannotPullContainerError` in the ECS service events;
> rotate the secret value to recover. For production, prefer Option 2.

### Option 2 — Mirror to your own ECR (recommended for production)

Copy the images once into an ECR registry in your own account and point the
stack there. This is the recommended production path because:

- **Fast, same-region pulls** — no cross-region or public-internet fetch on
  every task placement.
- **Execution-role auth that never expires** — ECS pulls from ECR using the
  task execution role, so there is no registry token to rotate and no
  PAT-expiry surprise mid-deploy.
- **No internet egress required** — add an [ECR VPC endpoint] and your private
  subnets pull images without any route to the internet, which matters for
  regulated / data-sovereignty deployments.
- **Fetch is decoupled from rollout** — mirroring happens ahead of time, so a
  slow pull never blocks a stack update.

Mirror the 6 images (multi-arch preserved):

```bash
export AXONFLOW_GHCR_USER=your-ghcr-username
export AXONFLOW_GHCR_TOKEN=your-read-packages-token

./mirror-images-to-your-ecr.sh \
  123456789012.dkr.ecr.us-east-1.amazonaws.com/axonflow 9.9.0
```

Then deploy with:
- `ContainerImageRegistry = 123456789012.dkr.ecr.us-east-1.amazonaws.com/axonflow`
- `RegistryCredentialsSecretArn =` *(leave empty)* — ECS pulls from ECR via the
  execution role.

[ECR VPC endpoint]: https://docs.aws.amazon.com/AmazonECR/latest/userguide/vpc-endpoints.html

---

## Key parameters

| Parameter | Notes |
|-----------|-------|
| `OrganizationID` | Your organization identifier (lowercase, hyphens). Required. |
| `VpcId`, `PublicSubnet1/2`, `PrivateSubnet1/2` | Your network (see prerequisites). Required. |
| `DBPassword` | RDS master password. Required. |
| `ContainerImageRegistry` | `ghcr.io/getaxonflow` (Option 1) or your ECR registry (Option 2). |
| `RegistryCredentialsSecretArn` | Secrets Manager ARN of `{"username","password"}` for GHCR-direct; **empty** for ECR. |
| `AgentImageTag` … `GrafanaImageTag` | v-prefixed release image tag (e.g. `v9.9.0`). Default `v9.9.0`. Release images are published v-prefixed. |
| `AgentDesiredCount`, `OrchestratorDesiredCount` | Replica counts. The agent is replica-interchangeable; the orchestrator is a required service. |
| `CustomerPortalDesiredCount`, `CustomerPortalUIDesiredCount` | Set to `0` to disable. |
| `DeployPrometheus`, `DeployGrafana` | Optional monitoring. |
| `LoadBalancerScheme` | `internal` (default) or `internet-facing`. |
| `DomainName` / `CertificateArn` | Optional HTTPS. |

---

## Deploy

The template is larger than the CloudFormation inline body limit, so upload it
to an S3 bucket in your account and deploy by URL:

```bash
REGION=us-east-1
BUCKET=my-cfn-templates          # a bucket you own in $REGION
aws s3 cp cloudformation-ecs-fargate.yaml "s3://$BUCKET/axonflow/cloudformation-ecs-fargate.yaml"

aws cloudformation create-stack \
  --region "$REGION" \
  --stack-name axonflow-prod \
  --template-url "https://$BUCKET.s3.amazonaws.com/axonflow/cloudformation-ecs-fargate.yaml" \
  --capabilities CAPABILITY_IAM CAPABILITY_NAMED_IAM \
  --parameters \
    ParameterKey=OrganizationID,ParameterValue=your-org \
    ParameterKey=VpcId,ParameterValue=vpc-xxxxxxxx \
    ParameterKey=PublicSubnet1,ParameterValue=subnet-aaaa \
    ParameterKey=PublicSubnet2,ParameterValue=subnet-bbbb \
    ParameterKey=PrivateSubnet1,ParameterValue=subnet-cccc \
    ParameterKey=PrivateSubnet2,ParameterValue=subnet-dddd \
    ParameterKey=DBPassword,ParameterValue='ChangeMe-Strong-Passw0rd!' \
    ParameterKey=ContainerImageRegistry,ParameterValue=ghcr.io/getaxonflow \
    ParameterKey=RegistryCredentialsSecretArn,ParameterValue=arn:aws:secretsmanager:us-east-1:123456789012:secret:axonflow/ghcr-pull-XXXX

aws cloudformation wait stack-create-complete --region "$REGION" --stack-name axonflow-prod
```

For the mirror-to-ECR path, set `ContainerImageRegistry` to your ECR registry
and omit `RegistryCredentialsSecretArn` (or pass an empty value).

---

## Verify

```bash
# Stack outputs (endpoints)
aws cloudformation describe-stacks --region "$REGION" --stack-name axonflow-prod \
  --query 'Stacks[0].Outputs' --output table

# ECS services should show runningCount == desiredCount
CLUSTER=$(aws cloudformation describe-stack-resources --region "$REGION" --stack-name axonflow-prod \
  --query 'StackResources[?ResourceType==`AWS::ECS::Cluster`].PhysicalResourceId' --output text)
aws ecs list-services --region "$REGION" --cluster "$CLUSTER" --output table
```

The agent and orchestrator expose `/health`. With an **internal** load
balancer these endpoints are reachable only from inside the VPC (e.g. via a
bastion or SSM port-forward); with `LoadBalancerScheme=internet-facing` they are
reachable from your workstation.

If images fail to pull, look at the **ECS service events** — a bad or expired
GHCR credential shows as `CannotPullContainerError` there (it does not appear in
the CloudFormation stack events):

```bash
aws ecs describe-services --region "$REGION" --cluster "$CLUSTER" \
  --services <service-arn> --query 'services[0].events[:10].message' --output table
```

---

## Upgrade

For the mirror-to-ECR path, `upgrade.sh` mirrors the new version, flips only the
6 image-tag parameters (keeping every other parameter unchanged), rolls the
update, and re-checks health:

```bash
cp upgrade.env.example upgrade.env    # set STACK_NAME + ECR_REGISTRY
./upgrade.sh 9.9.0
```

It is idempotent — re-running with the same version is a no-op. Always upgrade
the **whole stack** to a single version rather than bumping one image, so all
services stay in lock-step.

For the GHCR-direct path, upgrade by running `update-stack` with the new image
tags (and `RegistryCredentialsSecretArn` unchanged).

---

## Licensing

AxonFlow is **source-available** under the Business Source License (BSL 1.1).
The images deployed here are the licensed Enterprise build; access requires the
registry credentials in your welcome bundle.
