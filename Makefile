# Top-level Makefile — convenience wrappers around docker compose.
#
# Most molecule-core dev work happens via these shortcuts. CI doesn't
# use this Makefile; CI calls docker compose / go test directly so the
# Makefile can evolve without breaking the build.

.PHONY: help dev up down logs build test e2e-peer-visibility openapi-spec openapi-spec-check

help: ## Show this help.
	@grep -E '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-22s\033[0m %s\n", $$1, $$2}'

dev: ## Start the full stack with air hot-reload for the platform service.
	docker compose -f docker-compose.yml -f docker-compose.dev.yml up

up: ## Start the full stack in production-shape mode (no air, normal Dockerfile).
	docker compose up

down: ## Stop the stack and remove containers (volumes preserved).
	docker compose down

logs: ## Tail logs from all services (Ctrl-C to detach).
	docker compose logs -f

build: ## Force a fresh build of the platform image (no cache).
	docker compose build --no-cache platform

test: ## Run Go unit tests in workspace-server/.
	cd workspace-server && go test -race ./...

# ─── Local prod-mimic E2E gates ────────────────────────────────────────
# Run the LITERAL peer-visibility MCP list_peers gate against the
# already-running local stack (`make up` or `make dev`). Same byte-
# identical assertion as the staging gate — only provisioning differs.
# Skips any runtime whose provider key is absent (partially-keyed env
# is fine). See tests/e2e/test_peer_visibility_mcp_local.sh for the
# env contract (CLAUDE_CODE_OAUTH_TOKEN / E2E_MINIMAX_API_KEY / etc).
e2e-peer-visibility: ## Run the LOCAL peer-visibility MCP gate vs the running stack (needs `make up` first).
	bash tests/e2e/test_peer_visibility_mcp_local.sh

# ─── OpenAPI spec generation (RFC #1706, Phase 1) ─────────────────────
# Regenerate workspace-server/docs/openapi/swagger.{yaml,json} from
# swaggo annotations on the gin handlers. Commit the output. CI runs
# `make openapi-spec-check` to assert no drift between annotations and
# the committed file — if a PR changes a handler but forgets to
# regenerate, CI fails with a diff.
openapi-spec: ## Regenerate OpenAPI spec from workspace-server handler annotations.
	@command -v swag >/dev/null 2>&1 || go install github.com/swaggo/swag/cmd/swag@v1.16.4
	cd workspace-server && swag init \
	  --generalInfo cmd/server/main.go \
	  --output docs/openapi \
	  --outputTypes yaml,json \
	  --dir . \
	  --parseDependency=false \
	  --parseInternal=true

openapi-spec-check: openapi-spec ## CI gate — fail if openapi-spec produces a diff vs the committed file.
	@git diff --exit-code -- workspace-server/docs/openapi/ \
	  || (echo "openapi-spec is stale — run 'make openapi-spec' and commit the result" && exit 1)
