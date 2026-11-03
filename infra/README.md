# infra

Terraform for the optional AWS sandbox, a disposable test environment that is never production. Local runs never need it: `make up` uses Docker Compose ([`docs/plan.md`, section 3](../docs/plan.md#3-local-stack)).

This repository holds two Terraform environments: the **foundation** shared by all three services, and **Corridor's own services**. The ledger and Ditto deploy themselves from their own repositories ([ADR 0011](../docs/adr/0011-shared-foundation-through-ssm.md)).

## Foundation

| Module | Resources |
| --- | --- |
| `network` | A VPC across two availability zones, public and private subnets, one NAT gateway |
| `cluster` | The ECS cluster (Fargate, ARM64) and a Cloud Map namespace for service discovery |
| `github-oidc` | The GitHub OIDC provider; each repository creates its own deploy role trusting it |

It publishes its outputs to SSM Parameter Store, which is the contract with the other repositories:

| Parameter | Value |
| --- | --- |
| `/sandbox/foundation/vpc-id`, `/sandbox/foundation/vpc-cidr`, `/sandbox/foundation/private-subnet-ids`, `/sandbox/foundation/public-subnet-ids` | Network |
| `/sandbox/foundation/cluster-arn` | ECS cluster |
| `/sandbox/foundation/namespace-id`, `/sandbox/foundation/namespace-name` | Service discovery |
| `/sandbox/foundation/oidc-provider-arn` | GitHub OIDC provider |

## Corridor's services

| Module | Resources |
| --- | --- |
| `database` | Corridor's own Aurora PostgreSQL 17 Serverless v2 cluster, reachable only from Corridor's services' security groups |
| `service` | ECS services `fx-api`, `fx-worker` and `fx-dispatch`, and a migration task run by the deploy pipeline |
| `edge` | A public load balancer for the customer port; the internal port (operators, webhooks, console) stays private |
| `temporal` | A Temporal Cloud namespace (`temporalio/temporalcloud` provider) with its retention and search attributes; workers authenticate with an API key |
| `secrets` | Corridor's own secrets: the database roles' passwords and the auditor and operator keys, generated; the Temporal API key, which an operator fills because Temporal Cloud mints it. The secrets Corridor uses but others verify (its ledger client, Ditto's provider key and webhook secret) are created by the ledger and Ditto, which publish their ARNs in SSM; Corridor's execution roles may read exactly those ARNs |
| `deploy-role` | A role trusted by the foundation's OIDC provider for this repository's `v*` tags only, allowed to register task definitions, run the migration task and update Corridor's services. The release job that assumes it arrives with the sandbox deployment (I3, v2); until then a person applies and runs the migration task |

It reads the ledger's and Ditto's endpoints from `/sandbox/ledger/endpoint` and `/sandbox/ditto/endpoint`. The migration task connects as the master user, whose password RDS keeps in Secrets Manager, and sets the `fx_app` and `fx_audit` passwords from their secrets on every run.

## Layout

```
infra/
├── modules/
│   ├── network/  cluster/  github-oidc/
│   └── database/  service/  edge/  temporal/  secrets/  deploy-role/
└── envs/
    ├── foundation/       the shared foundation
    └── sandbox/          Corridor's services
```

## Order

Copy `backend.hcl.example` to `backend.hcl` with the state bucket, then initialize each environment with `terraform -chdir=envs/<env> init -backend-config=../../backend.hcl`.

1. Apply `envs/foundation`.
2. Apply the ledger's and Ditto's `infra/envs/sandbox`, in either order.
3. Apply `envs/sandbox` here.

Destroy in the reverse order. State lives in an encrypted S3 bucket with S3-native locking, one state per environment and repository; no repository reads another's state.

## Checks

| Check | Needs credentials |
| --- | --- |
| `make tf-validate`: `terraform fmt -check`, `terraform validate` without a backend, `tflint`, for both environments | No; runs on every pull request |
| `terraform plan` and `apply` | AWS and Temporal Cloud |

Generated secrets come from ephemeral resources written through write-only arguments, so no secret value appears in a plan or in state. No secret goes into a `.tfvars` file.

## Cost

A rough monthly estimate for the whole sandbox (all three repositories) left running, at `us-east-1` on-demand list prices. Check the AWS pricing calculator before applying.

| Item | About |
| --- | --- |
| NAT gateway | US$33, plus data processed |
| Application load balancer | US$17, plus capacity units |
| Public IPv4 addresses (load balancer and NAT) | US$11 |
| Fargate ARM64: five services at 0.25 vCPU and 0.5 GB | US$36 |
| Aurora Serverless v2, three clusters | US$0.12 per capacity-unit hour while active. Corridor's dispatcher and Ditto's scheduler poll every second or less, so those two clusters stay at their 0.5-unit floor while the services run, about US$44 each; the ledger's pauses when idle. Storage at US$0.10 per GB-month |
| Secrets Manager, CloudWatch Logs | US$10 |
| **Total** | **About US$190 a month**; a few hours for an evidence run cost a few dollars |

Temporal Cloud is billed separately by actions and storage; check its current pricing. A self-hosted Temporal cluster on the same ECS cluster is the cheaper alternative, at the cost of operating it. Destroy the sandbox after collecting evidence.
