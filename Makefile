GOLANGCI_LINT_VERSION := v2.15.0
ACTIONLINT_VERSION := 1.7.12
TERRAFORM_IMAGE := hashicorp/terraform:1.16.6
TFLINT_IMAGE := ghcr.io/terraform-linters/tflint:v0.64.0
GOMODCACHE := $(shell go env GOMODCACHE)

.PHONY: up down test lint openapi openapi-check image tf-validate

up: .env
	docker compose up --detach --build --wait

down:
	docker compose down --volumes

test:
	go test -race -count=1 ./...

lint:
	docker run --rm -v "$(CURDIR)":/src -v "$(GOMODCACHE)":/go/pkg/mod -w /src golangci/golangci-lint:$(GOLANGCI_LINT_VERSION) golangci-lint run
	docker run --rm -v "$(CURDIR)":/repo -w /repo rhysd/actionlint:$(ACTIONLINT_VERSION) -color

openapi:
	go tool vacuum lint --ruleset openapi/vacuum-ruleset.yaml --ignore-file openapi/vacuum-ignore.yaml --fail-severity warn --no-style openapi/ditto.yaml
	go tool oapi-codegen -config oapi-codegen.yaml openapi/ditto.yaml

openapi-check: openapi
	git diff --exit-code -- internal/api/types.gen.go

image:
	docker build --tag ditto:local .

.env:
	@{ for name in DITTO_ADMIN_KEY DITTO_KEY_CORRIDOR; do \
		echo "$$name=$$(openssl rand -hex 32)"; \
	done; echo "DITTO_WEBHOOK_SECRET_CORRIDOR=whsec_$$(openssl rand -base64 32)"; } > $@
	@echo "generated local keys in .env"

tf-validate:
	docker run --rm -v "$(CURDIR)/infra":/infra -w /infra --entrypoint sh $(TERRAFORM_IMAGE) -c '\
		terraform fmt -check -recursive && \
		for env in envs/*/; do \
			terraform -chdir=$$env init -backend=false -input=false >/dev/null && terraform -chdir=$$env validate || exit 1; \
		done'
	docker run --rm -v "$(CURDIR)/infra":/infra -v tflint-plugins:/root/.tflint.d -w /infra -e GITHUB_TOKEN --entrypoint sh $(TFLINT_IMAGE) -c '\
		tflint --init --config=/infra/.tflint.hcl && tflint --recursive --config=/infra/.tflint.hcl'
