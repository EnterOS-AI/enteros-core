# cf-proxy harness image — nginx + the harness's tenant-routing config baked
# in at build time.
#
# Why bake (not bind-mount): on Gitea Actions / act_runner, the runner is a
# container talking to the OUTER docker daemon over the host socket; runc
# resolves bind-mount source paths on the outer host filesystem, where the
# repo at `/workspace/.../tests/harness/cf-proxy/nginx.conf` is invisible.
# Compose `configs:` (with `file:`) falls back to bind mounts when swarm is
# not active, so it hits the same gap. A build-time COPY uploads the file
# as part of the docker build context — the daemon receives the tarball
# directly and never bind-mounts. See issue #88 item 2.
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
FROM ${BASE_IMAGE_REGISTRY}/nginx:1.27-alpine@sha256:62223d644fa234c3a1cc785ee14242ec47a77364226f1c811d2f669f96dc2ac8

COPY nginx.conf /etc/nginx/nginx.conf
