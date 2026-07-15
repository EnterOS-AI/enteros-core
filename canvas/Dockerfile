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
FROM ${BASE_IMAGE_REGISTRY}/node:22-alpine@sha256:d1c1bd9c83aa925a17a60442e9cf7f31e801643d55d99083383a0c760567404b AS builder
WORKDIR /app
COPY package.json package-lock.json* ./
# `npm ci` (not `install`) for lockfile-exact reproducibility.
# `--include=optional` ensures the platform-specific @tailwindcss/oxide
# native binary lands — without it, postcss fails with "Cannot read
# properties of undefined (reading 'All')" at build time.
RUN npm ci --include=optional
COPY . .
ARG NEXT_PUBLIC_PLATFORM_URL=http://localhost:8080
ARG NEXT_PUBLIC_WS_URL=ws://localhost:8080/ws
# Local docker-compose / ephemeral-E2E compatibility only. The production
# publish workflow leaves this empty: NEXT_PUBLIC values are public client
# JavaScript, so never pass a tenant ADMIN_TOKEN here.
ARG NEXT_PUBLIC_ADMIN_TOKEN=
# NEXT_PUBLIC_IMAGE_GEN_R2_HOST pins the exact generated-image R2 origin into
# the CSP img-src (buildImgSrc() in src/middleware.ts). Inlined by Next.js at
# build time. Unset → buildImgSrc() falls back to the wildcard. The combined
# tenant image (workspace-server/Dockerfile.tenant) is the primary serving
# path; this standalone canvas image (docker-compose / self-host) gets it too
# for parity.
ARG NEXT_PUBLIC_IMAGE_GEN_R2_HOST=
ENV NEXT_PUBLIC_PLATFORM_URL=$NEXT_PUBLIC_PLATFORM_URL
ENV NEXT_PUBLIC_WS_URL=$NEXT_PUBLIC_WS_URL
ENV NEXT_PUBLIC_ADMIN_TOKEN=$NEXT_PUBLIC_ADMIN_TOKEN
ENV NEXT_PUBLIC_IMAGE_GEN_R2_HOST=$NEXT_PUBLIC_IMAGE_GEN_R2_HOST
RUN npm run build

ARG BASE_IMAGE_REGISTRY
FROM ${BASE_IMAGE_REGISTRY}/node:22-alpine@sha256:d1c1bd9c83aa925a17a60442e9cf7f31e801643d55d99083383a0c760567404b
WORKDIR /app
COPY --from=builder /app/.next/standalone ./
COPY --from=builder /app/.next/static ./.next/static
COPY --from=builder /app/public ./public

EXPOSE 3000
ENV PORT=3000
ENV HOSTNAME="0.0.0.0"
# Git SHA the image was built from, surfaced at /api/buildinfo so canvas
# deploys are verifiable by the served SHA the same way workspace-server's
# /buildinfo is (core#2235). Wired from `${{ github.sha }}` in
# publish-canvas-image.yml. Server-only (not NEXT_PUBLIC_) — the route
# handler reads it at runtime on the standalone Node server, so it stays
# out of the client bundle. Set on the final stage (not the builder) so it
# lives in the runtime env that force-dynamic reads per request. Default
# "dev" matches the route + workspace-server sentinel: an unwired build
# fails the SHA comparison closed instead of looking deployed.
ARG BUILD_SHA=dev
ENV BUILD_SHA=$BUILD_SHA
# Non-root runtime — use addgroup/adduser without fixed GID/UID to avoid conflicts with base image
RUN addgroup canvas 2>/dev/null || true && adduser -G canvas -s /bin/sh -D canvas 2>/dev/null || true
USER canvas
CMD ["node", "server.js"]
