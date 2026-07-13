# Python tenant-CP simulator + canary test driver.
# Single image — pytest + httpx + the canary tests baked in.
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
FROM ${BASE_IMAGE_REGISTRY}/python:3.11-slim@sha256:920fec2d640e4cf366a1a20b350c5a94c67e8c6a5a1e1179ee2822306ef39370

WORKDIR /harness

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

# Test files are bind-mounted by docker-compose at run time so a `pytest -x`
# rerun loop doesn't require a rebuild. The COPY here is for the
# self-contained image used by Gitea Actions (where bind mounts are awkward).
COPY cp_sim.py /harness/cp_sim.py
COPY canary /harness/canary

ENV PYTHONUNBUFFERED=1

# Default: run the 4 canaries with verbose output + JUnit XML for CI.
CMD ["pytest", "-v", "--tb=short", "--junitxml=/harness/artifacts/junit.xml", "canary/"]
