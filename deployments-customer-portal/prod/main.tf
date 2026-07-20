terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "5.84.0"
    }
  }

  backend "s3" {
    bucket                 = "sre-tfstate-bukuwarung-v4-prod"
    key                    = "services/bukuwarung-v4-prod/prod/axonflow-customer-portal/terraform.tfstate"
    region                 = "ap-southeast-3"
    encrypt                = true
    dynamodb_table         = "terraform-lock"
    skip_region_validation = true
  }
}

variable "image_tag" {
  type        = string
  description = "Image tag (AxonFlow release tag, e.g. v9.9.0). The axonflow-customer-portal image must already exist in ECR at this tag (mirror it from GHCR with aws/mirror-images-to-your-ecr.sh)."
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

  # AxonFlow governance-console backend API. Pulled from ECR as
  # <account>.dkr.ecr.ap-southeast-3.amazonaws.com/axonflow-customer-portal:<image_tag>.
  image_name = "axonflow-customer-portal"
  image_tag  = var.image_tag

  aws_region = "ap-southeast-3"

  service     = "axonflow-customer-portal"
  environment = "prod"
  team        = "mxg"

  # Mirrors CFN CustomerPortalTaskDefinition (0.5 vCPU / 1 GB).
  fargate_cpu    = 512
  fargate_memory = 1024

  environment_variables = var.environment_variables
  secrets               = var.secrets

  app_count      = 1
  container_port = 8080

  enable_deployment_circuit_breaker = true

  # Datadog sidecar. The portal backend is a Go service.
  datadog_agent = {
    enabled          = true
    source           = "go"
    service_mapping  = ""
    version          = var.image_tag
    service_name     = "axonflow-customer-portal-bud-jk"
    dogstatsd_port   = 8125
    jmx_enabled      = false
    jmx_port         = 9012
    jmx_integrations = ["tomcat"]
  }

  # Healthcheck — curl is present in the portal image (see docker-compose.yml).
  health_check_cmd         = "curl -f http://localhost:8080/health || exit 1"
  health_check_interval    = 30
  health_check_timeout     = 5
  health_check_retries     = 10
  health_check_startperiod = 180

  lb_health_check_grace_period = 600

  # ECS timeout
  ecs_create_timeout = "60m"
  ecs_update_timeout = "60m"
  ecs_delete_timeout = "60m"

  # AUTOSCALING (portal is light; a small floor is plenty)
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

  # No aws_lb_target_group_name: the portal backend is internal-only. The UI
  # (server-side) and the agent reach it via Cloud Map at
  # axonflow-customer-portal.prod.bukuwarung-v4.local:8080. If you want the
  # portal API exposed on the ALB directly, create a target group + host rule
  # and set aws_lb_target_group_name here.

  # IAM Roles
  task_role_policy           = file("task-role-policy.json")
  task_execution_role_policy = file("task-execution-role-policy.json")

  # Capacity Provider
  fargate_base_count  = 1
  fargate_weight      = 1
  fargate_spot_weight = 0
}
