terraform {
  required_version = ">= 1.11"
  required_providers {
    aws = { source = "hashicorp/aws", version = "~> 6.0" }
  }
}

variable "name" {
  type = string
}

variable "cluster_arn" {
  type = string
}

variable "vpc_id" {
  type = string
}

variable "subnet_ids" {
  type = list(string)
}

variable "namespace_id" {
  type    = string
  default = null
}

variable "image" {
  type        = string
  description = "An image pinned by digest"
  validation {
    condition     = can(regex("@sha256:[0-9a-f]{64}$", var.image))
    error_message = "Pin the image by digest."
  }
}

variable "command" {
  type = list(string)
}

variable "cpu" {
  type    = number
  default = 256
}

variable "memory" {
  type    = number
  default = 512
}

variable "ports" {
  type    = list(number)
  default = []
}

variable "ingress_cidrs" {
  type    = list(string)
  default = []
}

variable "environment" {
  type    = map(string)
  default = {}
}

variable "secrets" {
  type        = map(string)
  default     = {}
  description = "Environment variable name to Secrets Manager ARN"
}

variable "health_command" {
  type        = list(string)
  default     = null
  description = "A container health check, so the deployment circuit breaker sees an unhealthy task"
}

variable "desired_count" {
  type        = number
  default     = 1
  description = "Zero creates only the task definition, for one-off tasks such as migrations"
}

variable "target_group_arn" {
  type    = string
  default = null
}

data "aws_region" "current" {}

locals {
  # The key is left out, not set to null, for a service without a health command.
  health_check = {
    for key, check in {
      healthCheck = { command = var.health_command, interval = 10, timeout = 3, retries = 3, startPeriod = 15 }
    } : key => check if var.health_command != null
  }
}

resource "aws_cloudwatch_log_group" "this" {
  name              = "/ecs/${var.name}"
  retention_in_days = 14
}

data "aws_iam_policy_document" "assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "execution" {
  name               = "${var.name}-execution"
  assume_role_policy = data.aws_iam_policy_document.assume.json
}

resource "aws_iam_role_policy_attachment" "execution" {
  role       = aws_iam_role.execution.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

# A valueFrom may select a JSON key (arn:...:secret:name:password::); IAM wants the secret's ARN.
data "aws_iam_policy_document" "secrets" {
  count = length(var.secrets) > 0 ? 1 : 0
  statement {
    actions   = ["secretsmanager:GetSecretValue"]
    resources = distinct([for value in values(var.secrets) : regex("^arn:[^:]+:secretsmanager:[^:]+:[^:]+:secret:[^:]+", value)])
  }
}

resource "aws_iam_role_policy" "secrets" {
  count  = length(var.secrets) > 0 ? 1 : 0
  name   = "read-secrets"
  role   = aws_iam_role.execution.id
  policy = data.aws_iam_policy_document.secrets[0].json
}

resource "aws_iam_role" "task" {
  name               = "${var.name}-task"
  assume_role_policy = data.aws_iam_policy_document.assume.json
}

resource "aws_ecs_task_definition" "this" {
  family                   = var.name
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = var.cpu
  memory                   = var.memory
  execution_role_arn       = aws_iam_role.execution.arn
  task_role_arn            = aws_iam_role.task.arn
  runtime_platform {
    cpu_architecture        = "ARM64"
    operating_system_family = "LINUX"
  }
  container_definitions = jsonencode([merge({
    name         = var.name
    image        = var.image
    command      = var.command
    essential    = true
    portMappings = [for port in var.ports : { containerPort = port, protocol = "tcp" }]
    environment  = [for key, value in var.environment : { name = key, value = value }]
    secrets      = [for key, arn in var.secrets : { name = key, valueFrom = arn }]
    logConfiguration = {
      logDriver = "awslogs"
      options = {
        awslogs-group         = aws_cloudwatch_log_group.this.name
        awslogs-region        = data.aws_region.current.region
        awslogs-stream-prefix = var.name
      }
    }
  }, local.health_check)])
}

resource "aws_security_group" "this" {
  name   = var.name
  vpc_id = var.vpc_id
  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
  dynamic "ingress" {
    for_each = length(var.ingress_cidrs) > 0 ? var.ports : []
    content {
      from_port   = ingress.value
      to_port     = ingress.value
      protocol    = "tcp"
      cidr_blocks = var.ingress_cidrs
    }
  }
}

resource "aws_service_discovery_service" "this" {
  count = var.desired_count > 0 && var.namespace_id != null && length(var.ports) > 0 ? 1 : 0
  name  = var.name
  dns_config {
    namespace_id = var.namespace_id
    dns_records {
      ttl  = 10
      type = "A"
    }
  }
}

resource "aws_ecs_service" "this" {
  count           = var.desired_count > 0 ? 1 : 0
  name            = var.name
  cluster         = var.cluster_arn
  task_definition = aws_ecs_task_definition.this.arn
  desired_count   = var.desired_count
  launch_type     = "FARGATE"
  network_configuration {
    subnets         = var.subnet_ids
    security_groups = [aws_security_group.this.id]
  }
  dynamic "service_registries" {
    for_each = aws_service_discovery_service.this
    content {
      registry_arn = service_registries.value.arn
    }
  }
  dynamic "load_balancer" {
    for_each = var.target_group_arn == null ? [] : [var.target_group_arn]
    content {
      target_group_arn = load_balancer.value
      container_name   = var.name
      container_port   = var.ports[0]
    }
  }
  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }
}

output "name" {
  value = var.name
}

output "task_role_arn" {
  value = aws_iam_role.task.arn
}

output "execution_role_arn" {
  value = aws_iam_role.execution.arn
}

output "security_group_id" {
  value = aws_security_group.this.id
}
