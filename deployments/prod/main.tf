terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "5.84.0"
    }
  }

  backend "s3" {
    bucket                 = "sre-tfstate-bukuwarung-v4-prod"
    key                    = "services/bukuwarung-v4-prod/prod/axonflow-agent/terraform.tfstate"
    region                 = "ap-southeast-3"
    encrypt                = true
    dynamodb_table         = "terraform-lock"
    skip_region_validation = true
  }
}

variable "image_tag" {
  type        = string
  description = "Image tag (AxonFlow release tag, e.g. v9.9.0). The axonflow-agent image must already exist in ECR at this tag (mirror it from GHCR with aws/mirror-images-to-your-ecr.sh)."
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

  # AxonFlow data-plane (policy enforcement gateway). Pulled from ECR as
  # <account>.dkr.ecr.ap-southeast-3.amazonaws.com/axonflow-agent:<image_tag>.
  image_name = "axonflow-agent"
  image_tag  = var.image_tag

  aws_region = "ap-southeast-3"

  service     = "axonflow-agent"
  environment = "prod"
  team        = "mxg"

  # Mirrors CFN AgentTaskDefinition (1 vCPU / 2 GB).
  fargate_cpu    = 1024
  fargate_memory = 2048

  environment_variables = var.environment_variables
  secrets               = var.secrets

  app_count      = 2
  container_port = 8080

  enable_deployment_circuit_breaker = true

  # Datadog sidecar (replaces the bundled Prometheus/Grafana stack). AxonFlow
  # agent is a Go service, so DogStatsD/APM autoinstrumentation uses "go".
  datadog_agent = {
    enabled          = true
    source           = "go"
    service_mapping  = ""
    version          = var.image_tag
    service_name     = "axonflow-agent-bud-jk"
    dogstatsd_port   = 8125
    jmx_enabled      = false
    jmx_port         = 9012
    jmx_integrations = ["tomcat"]
  }

  # Healthcheck — curl is present in the agent image (see docker-compose.yml).
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

  # AUTOSCALING
  enable_autoscaling                   = true
  min_scale_capacity                   = 2
  max_scale_capacity                   = 6
  default_scale_in_cooldown_seconds    = 300
  default_scale_out_cooldown_seconds   = 60
  target_cpu_utilization_percentage    = 70
  target_memory_utilization_percentage = 80

  # Infra Config (shared bukuwarung-v4-prod platform infrastructure)
  cluster_name                        = "bukuwarung-v4-prod-Cluster-kHgOTKQnhcBh"
  vpc_id                              = "vpc-0c8d0d4af930223ca"
  aws_service_discovery_dns_namespace = "prod.bukuwarung-v4.local"
  aws_security_group_ids              = ["sg-029773da46abd5b84"]

  # The agent is the public data plane. Create an ALB target group
  # "ecs-axonflow-agent-prod" (target type ip, port 8080, health check /health)
  # and a host-based listener rule on the existing 443 listener (e.g.
  # axonflow.bukuwarung.com -> this target group). The module registers agent
  # tasks into this target group.
  aws_lb_target_group_name = "ecs-axonflow-agent-prod"

  # EFS — durable audit-fallback storage for the agent (governance compliance:
  # audit rows that cannot reach Aurora are spooled to /mnt/efs/audit and
  # replayed). Provision an EFS filesystem + access point for AxonFlow (uid/gid
  # 1001 = the 'axonflow' container user) and fill the IDs below.
  efs_filesystem = [{
    name                                 = "axonflow-agent-efs"
    file_system_id                       = "fs-REPLACE_WITH_AXONFLOW_EFS_ID"
    root_directory                       = "/"
    transit_encryption                   = "ENABLED"
    authorization_config_iam             = "ENABLED"
    authorization_config_access_point_id = "fsap-REPLACE_WITH_AXONFLOW_ACCESS_POINT_ID"
  }]
  efs_subnets = ["subnet-030754c8113100253", "subnet-0a7a870960e520026"]

  mount_points = [{
    containerPath = "/mnt/efs/audit"
    sourceVolume  = "axonflow-agent-efs"
  }]

  # IAM Roles
  task_role_policy           = file("task-role-policy.json")
  task_execution_role_policy = file("task-execution-role-policy.json")

  # Capacity Provider
  fargate_base_count  = 1
  fargate_weight      = 1
  fargate_spot_weight = 0
}
