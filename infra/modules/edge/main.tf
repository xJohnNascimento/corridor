terraform {
  required_version = ">= 1.11"
  required_providers {
    aws = { source = "hashicorp/aws", version = "~> 6.0" }
  }
}

variable "name" {
  type = string
}

variable "vpc_id" {
  type = string
}

variable "public_subnet_ids" {
  type = list(string)
}

variable "certificate_arn" {
  type        = string
  description = "An ACM certificate for the customer API's domain"
}

resource "aws_security_group" "this" {
  name   = "${var.name}-edge"
  vpc_id = var.vpc_id
  ingress {
    from_port   = 443
    to_port     = 443
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }
  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_lb" "this" {
  name                       = var.name
  load_balancer_type         = "application"
  subnets                    = var.public_subnet_ids
  security_groups            = [aws_security_group.this.id]
  drop_invalid_header_fields = true
}

# Only the customer port is public; operators, webhooks and the console stay on the private port.
resource "aws_lb_target_group" "customer" {
  name             = "${var.name}-customer"
  port             = 8080
  protocol         = "HTTP"
  protocol_version = "HTTP1"
  target_type      = "ip"
  vpc_id           = var.vpc_id
  health_check {
    path = "/healthz"
  }
}

resource "aws_lb_listener" "https" {
  load_balancer_arn = aws_lb.this.arn
  port              = 443
  protocol          = "HTTPS"
  ssl_policy        = "ELBSecurityPolicy-TLS13-1-2-2021-06"
  certificate_arn   = var.certificate_arn
  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.customer.arn
  }
}

output "target_group_arn" {
  value = aws_lb_target_group.customer.arn
}

output "dns_name" {
  value = aws_lb.this.dns_name
}
