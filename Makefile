# Everything you need is one of these. `make help` lists them.

SHELL      := /bin/bash
APP_PORT   ?= 8080
BASE_URL   ?= http://localhost:$(APP_PORT)
TEST_DB    ?= postgres://$(USER)@localhost:5432/wallet_test?sslmode=disable
DEV_DB     ?= postgres://$(USER)@localhost:5432/wallet_dev?sslmode=disable

.DEFAULT_GOAL := help

.PHONY: help
help: ## show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\n\033[1mtargets\033[0m\n"} \
	     /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
	@echo

.PHONY: up
up: ## bring up app + postgres in docker (one command)
	APP_PORT=$(APP_PORT) docker compose up --build -d --wait
	@echo "up at $(BASE_URL)  ·  logs $(BASE_URL)/logs  ·  dashboard $(BASE_URL)/dashboard"

.PHONY: down
down: ## tear down containers and volumes
	docker compose down -v

.PHONY: logs
logs: ## follow container logs
	docker compose logs -f app

.PHONY: burst
burst: ## run the invariant burst against $(BASE_URL)
	./scripts/burst.sh $(BASE_URL)

.PHONY: burst-heavy
burst-heavy: ## 1500 transfers at 100-way concurrency
	N_WALLET_RACERS=100 K_IDEMPOTENT=60 N_TRANSFERS=1500 N_PARALLEL=100 N_WALLETS=6 \
	  ./scripts/burst.sh $(BASE_URL)

.PHONY: test
test: ## run integration tests against a local postgres (race detector on)
	@createdb wallet_test 2>/dev/null || true
	TEST_DATABASE_URL="$(TEST_DB)" go test ./... -race -count=1

.PHONY: run
run: ## run the server locally against a local postgres
	@createdb wallet_dev 2>/dev/null || true
	DATABASE_URL="$(DEV_DB)" ADMIN_TOKEN=dev-admin-token PORT=$(APP_PORT) go run ./cmd/wallet

.PHONY: check
check: ## vet + gofmt + tests -- what CI runs
	go vet ./...
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "not gofmt-clean:"; echo "$$out"; exit 1; fi
	$(MAKE) test

.PHONY: invariants
invariants: ## ask the live service to audit its own invariants
	@curl -fsS $(BASE_URL)/invariants
	@echo
