terraform {
  required_version = ">= 1.11"
  required_providers {
    aws = { source = "hashicorp/aws", version = "~> 6.0" }
  }
}

# The provider is shared; each repository creates its own deploy role trusting it.
resource "aws_iam_openid_connect_provider" "github" {
  url            = "https://token.actions.githubusercontent.com"
  client_id_list = ["sts.amazonaws.com"]
}

output "provider_arn" {
  value = aws_iam_openid_connect_provider.github.arn
}
