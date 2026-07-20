terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "5.84.0"
    }
  }

  backend "s3" {
    bucket                 = "sre-tfstate-bukuwarung-v4-prod"
    key                    = "services/bukuwarung-v4-prod/prod/axonflow-customer-portal-ui/terraform.tfstate"
    region                 = "ap-southeast-3"
    encrypt                = true
    dynamodb_table         = "terraform-lock"
    skip_region_validation = true
  }
}

variable "image_tag" {
  type        = string
  description = "Image tag (AxonFlow release tag, e.g. v9.9.0). The axonflow-customer-portal-ui image must already exist in ECR at this tag (mirror it from GHCR with aws/mirror-images-to-your-ecr.sh)."
}

variable "environment_variables" {
  type = list(object({
    name  = string,
    value = string
  }))
  default = []
}

variable "secrets" {
  type = list(object({
    name      = string,
    valueFrom = string
  }))
  default = []
}

module "service" {
  source = "git::https://github.com/bukuwarung/srehub.git//terraform/modules/terraform-aws-ecs-service?ref=ecs-service-jk-1.1.0"

  # AxonFlow governance-console frontend (Next.js). Pulled from ECR as
  # <account>.dkr.ecr.ap-southeast-3.amazonaws.com/axonflow-customer-portal-ui:<image_tag>.
  image_name = "axonflow-customer-portal-ui"
  image_tag  = var.image_tag

  aws_region = "ap-southeast-3"

  service     = "axonflow-customer-portal-ui"
  environment = "prod"
  team        = "mxg"

  # Mirrors CFN CustomerPortalUITaskDefinition (0.25 vCPU / 512 MB).
  fargate_cpu    = 256
  fargate_memory = 512

  environment_variables = var.environment_variables
  secrets               = var.secrets

  app_count      = 1
  container_port = 3000

  enable_deployment_circuit_breaker = true

  # Datadog sidecar. The UI is a Next.js/Node app, so the source is "nodejs"
  # (unlike the Go agent/orchestrator/portal).
  datadog_agent = {
    enabled          = true
    source           = "nodejs"
    service_mapping  = ""
    version          = var.image_tag
    service_name     = "axonflow-customer-portal-ui-bud-jk"
    dogstatsd_port   = 8125
    jmx_enabled      = false
    jmx_port         = 9012
    jmx_integrations = ["tomcat"]
  }

  # Healthcheck — node-based probe (wget/curl not present in node:alpine).
  # Matches the CFN/compose /api/healthz probe.
  health_check_cmd         = "node -e \"require('http').get('http://localhost:3000/api/healthz',r=>{r.resume();process.exit(r.statusCode===200?0:1)}).on('error',()=>process.exit(1))\""
  health_check_interval    = 30
  health_check_timeout     = 5
  health_check_retries     = 10
  health_check_startperiod = 120

  lb_health_check_grace_period = 300

  # ECS timeout
  ecs_create_timeout = "60m"
  ecs_update_timeout = "60m"
  ecs_delete_timeout = "60m"

  # AUTOSCALING
  enable_autoscaling                   = true
  min_scale_capacity                   = 1
  max_scale_capacity                   = 3
  default_scale_in_cooldown_seconds    = 300
  default_scale_out_cooldown_seconds   = 60
  target_cpu_utilization_percentage    = 70
  target_memory_utilization_percentage = 80

  # Infra Config (shared bukuwarung-v4-prod platform infrastructure)
  cluster_name                        = "bukuwarung-v4-prod-Cluster-kHgOTKQnhcBh"
  vpc_id                              = "vpc-0c8d0d4af930223ca"
  aws_service_discovery_dns_namespace = "prod.bukuwarung-v4.local"
  aws_security_group_ids              = ["sg-029773da46abd5b84"]

  # The UI is the public web console users hit. Create an ALB target group
  # "ecs-axonflow-portal-ui-prod" (target type ip, port 3000, health check
  # /api/healthz) and a host-based listener rule on the existing 443 listener
  # (e.g. axonflow-console.bukuwarung.com -> this target group).
  aws_lb_target_group_name = "ecs-axonflow-portal-ui-prod"

  # IAM Roles
  task_role_policy           = file("task-role-policy.json")
  task_execution_role_policy = file("task-execution-role-policy.json")

  # Capacity Provider
  fargate_base_count  = 1
  fargate_weight      = 1
  fargate_spot_weight = 0
}
