# infra

Terraform that deploys Ditto to the shared AWS sandbox, a disposable test environment. It refuses any environment other than a sandbox: Ditto never runs in production.

The sandbox foundation (network, ECS cluster, service discovery, GitHub OIDC provider) belongs to Corridor's `infra/` and is published in SSM Parameter Store under `/sandbox/foundation/`. Apply it first.

## What it creates

| Resource | Notes |
| --- | --- |
| Aurora PostgreSQL 17 Serverless v2 | Ditto's own cluster, scaling to zero when idle. It accepts connections only from Ditto's own security groups (the service and the migration task) |
| ECS Fargate (ARM64) service `ditto` | Private; registered in the foundation's service discovery namespace |
| Migration task | An ECS task running `ditto migrate`, run as a one-off ECS task before the service updates. There is no deploy pipeline yet |
| Secrets Manager entries | The admin key, Corridor's provider key and its webhook signing secret. Ditto verifies the keys and signs the webhooks, so it owns them; the provider key's and webhook secret's ARNs are published under `/sandbox/ditto/clients/corridor/` for Corridor's infra to read. Ditto connects as the master user, whose password RDS keeps in Secrets Manager |
| Deploy role | Trusted by the foundation's GitHub OIDC provider, for this repository only; it is for the v2 release job, since no deploy pipeline exists yet |
| SSM parameter `/sandbox/ditto/endpoint` | Read by Corridor's `infra/` |

Webhooks go to Corridor's internal service-discovery name, `fx-api.<namespace>:8081/webhooks/ditto`, which is known from the foundation, so Ditto does not wait for Corridor's deployment.

## Layout

```
infra/
├── modules/             database, service, secrets, deploy-role: copies of Corridor's modules
└── envs/sandbox/        main.tf, backend.tf
```

## Checks

| Check | Needs credentials |
| --- | --- |
| `make tf-validate`: `terraform fmt -check`, `terraform validate` without a backend, `tflint` | No; runs on every pull request |
| `terraform plan` and `apply` in `envs/sandbox` | Yes |
