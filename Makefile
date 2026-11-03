SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c

GOLANGCI_LINT_VERSION := v2.15.0
ACTIONLINT_VERSION := 1.7.12
SHELLCHECK_VERSION := v0.11.0
TERRAFORM_IMAGE := hashicorp/terraform:1.16.6
TFLINT_IMAGE := ghcr.io/terraform-linters/tflint:v0.64.0
GOMODCACHE := $(shell go env GOMODCACHE)
DEV := -f compose.yaml -f compose.dev.yaml
STACK ?= up
LOAD_ENV := set -a && . $(CURDIR)/.env && set +a
GOLANGCI_LINT = docker run --rm -v "$(CURDIR)":/src -v "$(GOMODCACHE)":/go/pkg/mod -w /src/$(1) golangci/golangci-lint:$(GOLANGCI_LINT_VERSION) golangci-lint run

.PHONY: up up-dev down test lint verification-imports proto proto-check spike image seed demo scenarios replay replay-break providertest verify tf-validate

up: .env
	docker compose up --detach --build --wait

up-dev: .env
	docker compose $(DEV) up --detach --build --wait

down: .env
	docker compose --profile spike down --volumes

test:
	go test -race -count=1 ./...
	cd verification && go test -race -count=1 ./...
	cd scenarios && go vet ./...

lint: verification-imports
	$(call GOLANGCI_LINT,.)
	$(call GOLANGCI_LINT,scenarios)
	$(call GOLANGCI_LINT,verification)
	go tool workflowcheck ./...
	docker run --rm -v "$(CURDIR)":/repo -w /repo rhysd/actionlint:$(ACTIONLINT_VERSION) -color
	docker run --rm -v "$(CURDIR)":/mnt -w /mnt koalaman/shellcheck:$(SHELLCHECK_VERSION) .github/scripts/*.sh

# The checker proves the system from its exports, so it must not share Corridor's code. The
# dependency list goes to a file first: a failing go list must fail the check, not pass it.
verification-imports:
	@deps=$$(mktemp) && trap 'rm -f "$$deps"' EXIT && \
		(cd verification && go list -deps -test ./... > "$$deps") && \
		if grep -q 'corridor/internal' "$$deps"; then echo "verification must not import corridor/internal"; exit 1; fi

proto:
	go tool buf lint
	go tool buf generate

proto-check: proto
	git diff --exit-code -- api

spike: .env
	docker compose --profile spike up --detach --wait temporal-spike
	TEMPORAL_ADDRESS=127.0.0.1:7243 go test -tags spike -count=1 -v ./test/spike/; status=$$?; \
	docker compose --profile spike rm --stop --force temporal-spike; exit $$status

image:
	docker build --tag corridor-fx:local .

.env:
	@{ for name in LEDGER_SECRET_CORRIDOR_WORKER LEDGER_SECRET_CORRIDOR_AUDIT LEDGER_SECRET_SEED LEDGER_SECRET_OPERATOR \
		DITTO_ADMIN_KEY DITTO_KEY_CORRIDOR DITTO_KEY_CERTIFICATION FX_AUDITOR_KEY FX_OPERATOR_KEY_ALICE FX_OPERATOR_KEY_BOB \
		FX_CUSTOMER_KEY_ACME FX_CUSTOMER_KEY_TINYCO; do \
		echo "$$name=$$(openssl rand -hex 32)"; \
	done; echo "DITTO_WEBHOOK_SECRET_CORRIDOR=whsec_$$(openssl rand -base64 32)"; } > $@
	@echo "generated local secrets in .env"

seed: .env
	cd scenarios && $(LOAD_ENV) && go run . seed

demo: .env
	$(MAKE) --no-print-directory $(STACK) seed
	cd scenarios && $(LOAD_ENV) && go run . demo
	@printf '\nAct 4. Proof\n'
	$(MAKE) --no-print-directory verify
	@out=$$(cd verification && go test -count=1 -v -run TestCorruptedSnapshotsFailTheirInvariant .) && printf '%s\n' "$$out" | grep -- '--- PASS: .*/'
	@printf '\nAct 5. Versioning\n'
	$(MAKE) --no-print-directory replay-break

scenarios: .env
	cd scenarios && $(LOAD_ENV) && go run . run --only "$(ONLY)" $(if $(VERIFY),--verify)

replay:
	go test -count=1 ./test/replay/

# Each run keeps its output: a compile error or a stray failure must not pass for the expected one.
# TMPRL1100 is the Temporal SDK's code for a nondeterminism error.
replay-break:
	@echo "an unversioned change must fail replay with a nondeterminism error:"
	@out=$$(mktemp) && trap 'rm -f "$$out"' EXIT && \
		if go test -count=1 -tags replaybreak ./test/replay/ > "$$out" 2>&1; then \
			echo "  replay passed"; exit 1; \
		fi && \
		if ! grep -qE 'TMPRL1100|nondeterministic' "$$out"; then \
			cat "$$out"; echo "  replay failed, but not with a nondeterminism error"; exit 1; \
		fi && \
		echo "  failed with a nondeterminism error, as it must"
	@echo "the same change behind workflow.GetVersion must pass:"
	@out=$$(mktemp) && trap 'rm -f "$$out"' EXIT && \
		if ! go test -count=1 -tags replaybreak_versioned ./test/replay/ > "$$out" 2>&1; then \
			cat "$$out"; exit 1; \
		fi && \
		echo "  passed"

providertest: .env
	$(LOAD_ENV) && go test -count=1 -tags providertest ./internal/provider/providertest/

verify: .env
	cd verification && $(LOAD_ENV) && go run ./cmd/verify

tf-validate:
	docker run --rm -v "$(CURDIR)/infra":/infra -w /infra --entrypoint sh $(TERRAFORM_IMAGE) -c '\
		terraform fmt -check -recursive && \
		for env in envs/*/; do \
			terraform -chdir=$$env init -backend=false -input=false >/dev/null && terraform -chdir=$$env validate || exit 1; \
		done'
	docker run --rm -v "$(CURDIR)/infra":/infra -v tflint-plugins:/root/.tflint.d -w /infra -e GITHUB_TOKEN --entrypoint sh $(TFLINT_IMAGE) -c '\
		tflint --init --config=/infra/.tflint.hcl && tflint --recursive --config=/infra/.tflint.hcl'
