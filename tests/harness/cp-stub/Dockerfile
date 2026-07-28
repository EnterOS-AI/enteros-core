# cp-stub — minimal CP stand-in for the local production-shape harness.
# See main.go for the rationale. Self-contained build, no module deps.

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
# ARGs consumed by a FROM must be declared BEFORE any FROM — an ARG declared
# inside a stage is stage-scoped and resolves EMPTY in the next FROM line
# ("invalid reference format"). Both bases are therefore global here and
# re-declared bare where they are used.
ARG GOLANG_BASE=golang:1.25-alpine@sha256:c4ea15b4a7912716eb362a022e2b12317762eca387423760bc59c0f9ae69423c
ARG ALPINE_BASE=alpine:3.20@sha256:91bed4737195a1a9657c53334c35b0cd1cef217402d3d128a3de5aaee4bb25aa
FROM ${BASE_IMAGE_REGISTRY}/${GOLANG_BASE} AS builder
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /cp-stub .

ARG BASE_IMAGE_REGISTRY
ARG ALPINE_BASE
FROM ${BASE_IMAGE_REGISTRY}/${ALPINE_BASE}
RUN apk add --no-cache ca-certificates
COPY --from=builder /cp-stub /cp-stub
EXPOSE 9090
ENTRYPOINT ["/cp-stub"]
