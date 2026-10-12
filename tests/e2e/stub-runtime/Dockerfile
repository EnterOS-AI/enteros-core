# Tiny stub runtime image for the local Docker-provisioner lifecycle e2e.
#
# It impersonates a real workspace runtime's platform contract (register +
# heartbeat + A2A message/send) with ZERO LLM/SDK weight so the lifecycle e2e
# (provision -> online -> restart-survive -> proxy-reach) runs in seconds and
# without the 2.5GB real claude-code image.
#
# Resolution trick (see tests/e2e/test_local_provision_lifecycle_e2e.sh):
# the local provisioner resolves runtime=claude-code via RegistryModeLocal,
# which is a `docker image inspect` cache-check on
#   molecule-local/workspace-template-claude-code:<gitea-HEAD-sha12>
# BEFORE it clones+builds. Pre-tagging THIS image to that exact cache tag makes
# the provisioner cache-hit the stub instead of building the real template.
#
# linux/amd64: the provisioner forces --platform=linux/amd64 for every workspace
# container (defaultImagePlatform, #1875) for parity with the amd64-only prod
# images. Build the stub amd64 too so the platforms match and Docker doesn't
# refuse the create with a manifest mismatch.
# Base images come from OUR registry, not Docker Hub.
#
# WHY: Hub's ANONYMOUS pull limit (~100/6h per IP) is shared by every CI job on
# the runner and we hold NO Hub credentials. When it is spent, EVERY open PR
# reds at once on `429 Too Many Requests ... /v2/library/...` — a failure with
# no relationship to any PR's diff. registry.moleculesai.app mirrors these and
# has no such limit.
#
# The digests below are the MIRROR's, and for most images they DIFFER from the
# Hub digest they replace. That is not a content change: `docker push` of a
# single-platform image re-wraps the manifest, so the digest of the WRAPPER
# moves while every layer digest stays identical. A Hub digest that happens to
# name a child manifest (golang, node:20, nginx) survives verbatim; one that
# named Hub's multi-arch INDEX (alpine, node:22, python) cannot, and had to be
# re-pinned. Do not "restore" the Hub digests — they do not exist in the mirror.
#
# Override (e.g. to refresh the mirror from upstream):
#   --build-arg BASE_IMAGE_REGISTRY=docker.io/library
ARG BASE_IMAGE_REGISTRY=registry.moleculesai.app/molecule-ai
FROM --platform=linux/amd64 ${BASE_IMAGE_REGISTRY}/python:3.12-alpine@sha256:aa679aa4eed6eb56c1dc6ad3f1b98b7d2d788fd961596779d188fdedad97fb38

# /configs is the named-volume mount point the provisioner attaches
# (ws-<id>-configs:/configs). The real entrypoint chowns it; the stub just
# needs the dir to exist so a missing-mount never trips it up.
RUN mkdir -p /configs /workspace

# PyYAML: the stub parses the delivered /configs/config.yaml `schedules:` block
# (the self-host graft output) to serve the runtime schedule grid, mirroring the
# real runtime's PyYAML-based seed_schedules_from_workspace_config. A cp312
# musllinux wheel is published, so this is a wheel install (no compiler needed).
RUN pip install --no-cache-dir pyyaml==6.0.2

WORKDIR /app
COPY server.py /app/server.py

EXPOSE 8000

# No gosu/agent-uid drop here — the stub does no privileged work and the e2e
# only cares about the platform contract, not the agent-uid security posture.
ENTRYPOINT ["python3", "/app/server.py"]
