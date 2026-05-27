# Bundle: ai-memory (engine, Rust) + custom SPA (ai-memory-ui) served at /web.
#
# Clones both PUBLIC repos at pinned refs (build-args) and produces ONE image.
# The same image serves all instances (personal/client) — only the push registry
# and the branding build-args (VITE_APP_NAME/TAGLINE) change.
#
# Kaniko-safe: no `RUN --mount=type=cache` (BuildKit-only). On docker/buildkit
# you can speed up the Rust build by adding cache mounts.
#
# Example (docker):
#   docker build images/ai-memory \
#     --build-arg AI_MEMORY_REF=feature/custom-web-ui \
#     --build-arg AI_MEMORY_UI_REF=main \
#     --build-arg VITE_APP_NAME=Memory --build-arg VITE_APP_TAGLINE="knowledge browser" \
#     -t registry.example.com/<grupo>/ai-memory:<ver>

ARG AI_MEMORY_REPO=https://github.com/djalmajr/ai-memory.git
ARG AI_MEMORY_REF=feature/custom-web-ui
ARG AI_MEMORY_UI_REPO=https://github.com/djalmajr/ai-memory-ui.git
ARG AI_MEMORY_UI_REF=main

# ---------------------------------------------------------------------------
# 1) SPA (SolidJS) → /ui/dist
# ---------------------------------------------------------------------------
FROM node:22-slim AS ui
ARG AI_MEMORY_UI_REPO
ARG AI_MEMORY_UI_REF
ARG VITE_APP_NAME="Memory"
ARG VITE_APP_TAGLINE="knowledge browser"
RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /ui
RUN git clone --depth 1 --branch "${AI_MEMORY_UI_REF}" "${AI_MEMORY_UI_REPO}" .
RUN npm ci
ENV VITE_APP_NAME=${VITE_APP_NAME}
ENV VITE_APP_TAGLINE=${VITE_APP_TAGLINE}
RUN npm run build   # i18n + tsr generate + tsc + vite build → /ui/dist

# ---------------------------------------------------------------------------
# 2) ai-memory (Rust) → /usr/local/bin/ai-memory
# ---------------------------------------------------------------------------
FROM rust:1.95-slim-bookworm AS builder
ARG AI_MEMORY_REPO
ARG AI_MEMORY_REF
RUN apt-get update \
 && apt-get install -y --no-install-recommends pkg-config build-essential curl ca-certificates git \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /work
RUN git clone --depth 1 --branch "${AI_MEMORY_REF}" "${AI_MEMORY_REPO}" .
# TAILWIND_SKIP=1 skips the Tailwind checksum step in build.rs (headless/CI).
RUN TAILWIND_SKIP=1 cargo build --release -p ai-memory-cli \
 && cp target/release/ai-memory /usr/local/bin/ai-memory

# ---------------------------------------------------------------------------
# 3) runtime — debian-slim, non-root, SPA em /web-ui
# ---------------------------------------------------------------------------
FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --uid 1000 --user-group --home-dir /data \
        --shell /usr/sbin/nologin ai-memory \
 && mkdir -p /data /web-ui \
 && chown -R ai-memory:ai-memory /data
COPY --from=builder /usr/local/bin/ai-memory /usr/local/bin/ai-memory
# Lifecycle hooks (setup-agent/install-hooks find them here).
COPY --from=builder /work/hooks /usr/local/share/ai-memory/hooks
# Custom SPA — served via `serve --web-ui-dir /web-ui` (comes from the chart args).
COPY --from=ui /ui/dist /web-ui
USER ai-memory
ENV AI_MEMORY_DATA_DIR=/data
WORKDIR /data
EXPOSE 49374
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD ["/usr/local/bin/ai-memory", "status"]
ENTRYPOINT ["/usr/local/bin/ai-memory"]
# Default standalone; the chart overrides `args` (includes --workspace and --web-ui-dir).
CMD ["serve", "--transport", "http", "--bind", "0.0.0.0:49374", "--enable-web", "--web-ui-dir", "/web-ui"]
