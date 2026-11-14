LOCAL_SCRIPTS := deploy/local/scripts

.PHONY: help up down smoke check test

help: ## Show available targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-8s %s\n", $$1, $$2}'

up: ## Create or update the local kind cluster with gateways and tracing
	@$(LOCAL_SCRIPTS)/up.sh

down: ## Delete the local cluster (PURGE=1 also removes generated secrets)
	@$(LOCAL_SCRIPTS)/down.sh

smoke: ## Run end-to-end smoke checks against the local cluster
	@$(LOCAL_SCRIPTS)/smoke.sh

check: ## Static checks: script syntax, chart rendering, collector config
	@$(LOCAL_SCRIPTS)/check.sh

test: ## Unit tests (no cluster needed)
	@python3 -m unittest discover -s tools/mock-llm -q
