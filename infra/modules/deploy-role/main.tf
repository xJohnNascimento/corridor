terraform {
  required_version = ">= 1.11"
  required_providers {
    aws = { source = "hashicorp/aws", version = "~> 6.0" }
  }
}

variable "repository" {
  type        = string
  description = "owner/name of the only repository allowed to assume the role"
}

variable "oidc_provider_arn" {
  type = string
}

variable "cluster_arn" {
  type = string
  validation {
    condition     = can(regex("^arn:[^:]+:ecs:[^:]+:[0-9]+:cluster/[^/]+$", var.cluster_arn))
    error_message = "cluster_arn must be an ECS cluster ARN."
  }
}

variable "families" {
  type        = list(string)
  description = "The task definition families, which are also the service names"
}

variable "pass_role_arns" {
  type        = list(string)
  description = "The task and execution roles the pipeline hands to ECS"
}

data "aws_iam_policy_document" "trust" {
  statement {
    actions = ["sts:AssumeRoleWithWebIdentity"]
    principals {
      type        = "Federated"
      identifiers = [var.oidc_provider_arn]
    }
    condition {
      test     = "StringEquals"
      variable = "token.actions.githubusercontent.com:aud"
      values   = ["sts.amazonaws.com"]
    }
    condition {
      test     = "StringLike"
      variable = "token.actions.githubusercontent.com:sub"
      values   = ["repo:${var.repository}:ref:refs/tags/v*"]
    }
  }
}

resource "aws_iam_role" "this" {
  name               = "deploy-${replace(var.repository, "/", "-")}"
  assume_role_policy = data.aws_iam_policy_document.trust.json
}

locals {
  # arn:aws:ecs:region:account:cluster/name, split into its prefix and the cluster's name.
  ecs     = split(":cluster/", var.cluster_arn)
  cluster = local.ecs[1]
}

# Task definitions are the only ECS resources that cannot be scoped by ARN.
data "aws_iam_policy_document" "deploy" {
  statement {
    actions   = ["ecs:RegisterTaskDefinition", "ecs:DescribeTaskDefinition"]
    resources = ["*"]
  }
  statement {
    actions   = ["ecs:UpdateService", "ecs:DescribeServices"]
    resources = [for family in var.families : "${local.ecs[0]}:service/${local.cluster}/${family}"]
  }
  statement {
    actions   = ["ecs:RunTask"]
    resources = [for family in var.families : "${local.ecs[0]}:task-definition/${family}:*"]
    condition {
      test     = "ArnEquals"
      variable = "ecs:cluster"
      values   = [var.cluster_arn]
    }
  }
  statement {
    actions   = ["ecs:DescribeTasks"]
    resources = ["${local.ecs[0]}:task/${local.cluster}/*"]
  }
  statement {
    actions   = ["iam:PassRole"]
    resources = var.pass_role_arns
    condition {
      test     = "StringEquals"
      variable = "iam:PassedToService"
      values   = ["ecs-tasks.amazonaws.com"]
    }
  }
}

resource "aws_iam_role_policy" "deploy" {
  name   = "deploy"
  role   = aws_iam_role.this.name
  policy = data.aws_iam_policy_document.deploy.json
}

output "arn" {
  value = aws_iam_role.this.arn
}
