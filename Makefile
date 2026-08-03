# Top-level Makefile — convenience wrappers around docker compose.
#
# Most molecule-core dev work happens via these shortcuts. CI doesn't
# use this Makefile; CI calls docker compose / go test directly so the
# Makefile can evolve without breaking the build.

.PHONY: help bundle-deps dev up down logs build build-upstream-base test e2e-peer-visibility e2e-concierge-creates-workspace e2e-ephemeral-happy-path e2e-ephemeral-boot e2e-ephemeral-scenario e2e-ephemeral-down openapi-spec openapi-spec-check gen gen-docker gen-check gen-check-docker

# ─── Provider-registry SSOT codegen (internal#718) ─────────────────────
# The Go module lives in workspace-server/. The checked-in artifact
# workspace-server/internal/providers/gen/registry_gen.go is a gofmt'd
# projection of providers.yaml, drift-gated by
# .gitea/workflows/verify-providers-gen.yml. `make gen-docker` runs the SAME
# generator inside the pinned golang image so a toolchain-less env (an agent
# without Go) can regenerate without a local Go install (core#2332 follow-up).
#
# BYTE-EQUIVALENCE: gen-docker is byte-identical to native only while
# GO_VERSION below matches the `go` directive in workspace-server/go.mod.
# NOTE: the CI verify workflow pins setup-go go-version: 'stable' (not '1.25');
# that is a latent hazard — a future Go minor could reformat the artifact in CI
# vs a 1.25 local. Pin CI to '1.25' to close it (tracked alongside this change).
GO_VERSION ?= 1.25
GO_IMAGE   ?= golang:$(GO_VERSION)
DOCKER     ?= docker
# Mount the Go module (workspace-server) read-write; Go's default -mod=readonly
# keeps go.mod/go.sum untouched — only the artifact is written in-place.
DOCKER_RUN_WS = $(DOCKER) run --rm -v "$(CURDIR)/workspace-server":/src -w /src $(GO_IMAGE)

help: ## Show this help.
	@grep -E '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-22s\033[0m %s\n", $$1, $$2}'

# ─── Manifest-managed template/plugin dirs ─────────────────────────────
# `.tenant-bundle-deps/` is a REQUIRED build input: workspace-server/Dockerfile
# COPYs .tenant-bundle-deps/{workspace-configs-templates,org-templates,plugins}
# into the image. Nothing in a fresh checkout creates it — the dirs are
# .gitignored and only CI populated them — so `make up` / `make build` failed
# on a clean clone with an opaque COPY error.
#
# The top-level workspace-configs-templates/ org-templates/ plugins/ dirs are
# the OTHER destination: docker-compose.yml bind-mounts them (CONFIGS_HOST_DIR,
# PLUGINS_HOST_DIR) so a running stack sees the template palette. An empty
# top-level dir is why Canvas silently shows no templates locally.
#
# clone-manifest.sh is idempotent — it skips any dir whose manifest-source
# marker still matches manifest.json — so re-running is a fast no-op and it is
# safe to hang off dev/up/build.
#
# Without a token this clones what is public and SKIPS repos the manifest marks
# `"private": true` (a contributor should not need creds for a local stack).
# Set MOLECULE_GITEA_TOKEN to populate the full palette.
bundle-deps: ## Populate .tenant-bundle-deps/ + the template dirs from manifest.json (idempotent).
	@command -v jq >/dev/null 2>&1 || { \
	  echo "make bundle-deps: jq is required (brew install jq / apt-get install jq)"; \
	  echo "  without it the template palette stays empty and the image build fails."; \
	  exit 1; }
	@# Strip JSON5-style // comments before jq sees the manifest (same as CI).
	@sed '/^[[:space:]]*\/\//d' manifest.json > .manifest-stripped.json
	@mkdir -p .tenant-bundle-deps
	bash scripts/clone-manifest.sh .manifest-stripped.json \
	  .tenant-bundle-deps/workspace-configs-templates \
	  .tenant-bundle-deps/org-templates \
	  .tenant-bundle-deps/plugins
	bash scripts/clone-manifest.sh .manifest-stripped.json \
	  workspace-configs-templates org-templates plugins
	@rm -f .manifest-stripped.json
	@echo "bundle-deps: image build input + bind-mounted template dirs are populated"

dev: bundle-deps ## Start the full stack with air hot-reload for the platform service.
	docker compose -f docker-compose.yml -f docker-compose.dev.yml up

up: bundle-deps ## Start the full stack in production-shape mode (no air, normal Dockerfile).
	docker compose up

# ─── arm64 / Apple Silicon escape hatch ────────────────────────────────
# The pinned base images live in a private mirror whose alpine:3.20 index
# carries ONLY linux/amd64, so `docker build` on an arm64 host dies with
# "no match for platform in manifest: not found" and molecule-core cannot be
# built on Apple Silicon at all.
#
# This target builds against the UPSTREAM Docker Hub bases instead. The
# mirrored amd64 image is alpine 3.20.10 — the same release as upstream — so
# this is a same-content, more-architectures swap, not a version bump.
#
# The durable fix is mirroring the arm64 variant so CI and local builds share
# one pinned reference again; that needs registry write access. CI keeps using
# the pinned mirror defaults either way — this target is local-only.
build-upstream-base: bundle-deps ## Build the platform image from UPSTREAM bases (arm64 / Apple Silicon).
	docker build \
	  --build-arg BASE_IMAGE_REGISTRY=docker.io/library \
	  --build-arg GOLANG_BASE=golang:1.25-alpine \
	  --build-arg ALPINE_BASE=alpine:3.20 \
	  -f workspace-server/Dockerfile \
	  -t molecule-core-platform:local .

down: ## Stop the stack and remove containers (volumes preserved).
	docker compose down

logs: ## Tail logs from all services (Ctrl-C to detach).
	docker compose logs -f

build: bundle-deps ## Force a fresh build of the platform image (no cache).
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

# FUNCTIONAL local proof that the org concierge actually DOES org-management:
# send it a natural-language A2A request and assert it really CREATES a workspace
# via its platform MCP (create_workspace) — the deterministic side effect, not a
# REST 200. The concierge row is seeded unconditionally on self-host boot
# (core#3496 — a MISSING concierge is a hard FAIL now, not a skip); the script
# still SKIPs LOUD (exit 0) when the stack can't run the functional leg (not
# online / not on the platform-agent image / no model key). To run it green
# locally: configure a model + key (onboarding scene, Settings, or
# MOLECULE_LLM_DEFAULT_MODEL + a provider key in env). See the script header.
e2e-concierge-creates-workspace: ## Prove the concierge actually creates a workspace via its platform MCP (skips loud if not runnable).
	bash tests/e2e/test_concierge_creates_workspace_local.sh

# RFC "one pre-merge ephemeral gate" (§04): run the FULL cross-boundary happy
# path against a THROWAWAY CP this target spins up itself — same scenario runner
# CI uses (tests/e2e/ephemeral_cp_happy_path.sh), against your working-tree tenant
# image. The local wrapper uses direct Docker + the sibling CP checkout by default;
# CI separately pins its CP ref and launches the topology inside per-job dind.
# No shared staging, no CI wait: validate before you push. Needs docker + a
# sibling molecule-controlplane checkout (or CP_EPHEMERAL_SCRIPT / CP_IMAGE set).
# See local-e2e/ephemeral-cp-happy-path.sh for the overridable env.
e2e-ephemeral-happy-path: ## Run the FULL happy-path scenario against a local throwaway CP (no staging).
	bash local-e2e/ephemeral-cp-happy-path.sh all

# MODULAR PHASES — pinpoint a failing scenario step without the full rebuild+boot.
# Boot ONCE (~minutes: build CP+tenant, boot CP, migrate), then re-run the
# scenario as many times as you like (~2 min each) while you fix; tear down when
# done. `scenario`/`down` NEVER rebuild — they attach to the standing CP.
e2e-ephemeral-boot: ## Boot a throwaway CP and LEAVE IT UP (prints how to run the scenario/down).
	bash local-e2e/ephemeral-cp-happy-path.sh boot

e2e-ephemeral-scenario: ## Re-run full-saas against the standing CP (fast, repeatable — no rebuild).
	bash local-e2e/ephemeral-cp-happy-path.sh scenario

e2e-ephemeral-down: ## Tear down the standing throwaway CP + its Postgres.
	bash local-e2e/ephemeral-cp-happy-path.sh down

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

# ─── Provider-registry codegen targets ────────────────────────────────
gen: ## Regenerate the providers registry artifact natively (needs local Go).
	cd workspace-server && go generate ./...

gen-docker: ## Same, inside the pinned $(GO_IMAGE) — Docker only, no local Go.
	$(DOCKER_RUN_WS) go generate ./...

gen-check: ## Drift gate (native): exit 1 if the artifact is stale.
	cd workspace-server && go run ./cmd/gen-providers -check

gen-check-docker: ## Drift gate inside the pinned $(GO_IMAGE) — Docker only.
	$(DOCKER_RUN_WS) go run ./cmd/gen-providers -check
