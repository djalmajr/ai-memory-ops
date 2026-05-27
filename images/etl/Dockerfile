# ai-memory-svc ETL image (ai-memory stack).
#
# Sidecar container that runs `supercronic` in a loop and triggers `run-etl.sh`
# on schedule (`ETL_SCHEDULE`). `run` mode (one-shot) is also available.
#
# Runtime stack:
# - node 24.x (npm — used by `npx -y skills my-org/skills`)
# - opencode CLI (binary from the GitHub release `anomalyco/opencode`)
# - supercronic (internal cron; PID 1, does native reaping)
# - git, curl, jq, ripgrep, bash
#
# There is no longer a separate indexer in this image: ai-memory is the source of truth and the ETL writes
# via HTTP (POST /admin/write-page + /admin/embed). No native modules to
# compile → single-stage, a much smaller image than the older stack (no node-llama-cpp /
# better-sqlite3 / tree-sitter / build-essential / python3).
#
# Build:
#   docker build --platform linux/amd64 -t ai-memory-svc-etl:0.2.0 images/etl/

FROM node:24-bookworm-slim

# Version label (image semver).
# 0.2.2: per-project embed in the loop (/admin/embed is scoped by workspace+project;
#        workspace-only embedded nothing).
# 0.2.1: per-source token (tokenEnv) — sources on distinct GitLabs (gitlab.example.com
#        vs gitlab.example.com) use different PATs.
# 0.2.0: ETL rewritten for the ai-memory stack — writes via
#        HTTP (/admin/write-page + /admin/embed); removes the old clone/push of
#        wiki-content. Single-stage image (no native modules from the old indexer).
# 0.1.7 and earlier: the older stack (see git history).
LABEL org.opencontainers.image.version=0.2.2 \
      org.opencontainers.image.title="ai-memory-svc-etl" \
      org.opencontainers.image.description="ETL (opencode ingest -> ai-memory HTTP) for the ai-memory-svc"

# TARGETARCH is set automatically by buildx (amd64 / arm64).
ARG TARGETARCH

# ============================================================================
# Pinned versions (binaries downloaded at build-time)
# ============================================================================
# supercronic — https://github.com/aptible/supercronic/releases/latest
ARG SUPERCRONIC_VERSION=0.2.45
ARG SUPERCRONIC_SHA256_amd64=bb6da5af8d5547c9a5cbb4cf58d9f5541f0433df2188bfe4f1a54b04ad253db6
ARG SUPERCRONIC_SHA256_arm64=c0f21174f7bb3c80a9b33567ba0cfbeb3e51e765fe9808267ba72a1ac88c3dba

# opencode — https://github.com/anomalyco/opencode/releases/latest
ARG OPENCODE_VERSION=1.15.4
ARG OPENCODE_SHA256_amd64=f0734928d5df360777f51f807df18b28c1d0c006f806ad0bd35a2420fabd0835
ARG OPENCODE_SHA256_arm64=978f070e280c36ea6fd9a03d64f813028dbc2434077ad5cb6aecf37423e156d7

# ============================================================================
# Minimal system packages
# - git: clone the sources
# - curl + ca-certificates: download artifacts (build) + POSTs to ai-memory (runtime)
# - jq: parse WIKI_SOURCES_JSON + build JSON payloads for /admin/write-page
# - ripgrep: used by the wiki-lint skill
# ============================================================================
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      git \
      curl \
      ca-certificates \
      jq \
      ripgrep \
 && rm -rf /var/lib/apt/lists/*

# ============================================================================
# supercronic (binary release) — arch-aware
# ============================================================================
RUN set -eux; \
    case "$TARGETARCH" in \
      amd64) SUP_ASSET=supercronic-linux-amd64 ; SUP_SHA="${SUPERCRONIC_SHA256_amd64}" ;; \
      arm64) SUP_ASSET=supercronic-linux-arm64 ; SUP_SHA="${SUPERCRONIC_SHA256_arm64}" ;; \
      *) echo "TARGETARCH '$TARGETARCH' not supported" >&2; exit 1 ;; \
    esac; \
    curl -fsSLo /usr/local/bin/supercronic \
      "https://github.com/aptible/supercronic/releases/download/v${SUPERCRONIC_VERSION}/${SUP_ASSET}"; \
    echo "${SUP_SHA}  /usr/local/bin/supercronic" | sha256sum -c -; \
    chmod +x /usr/local/bin/supercronic

# ============================================================================
# opencode CLI (binary release) — arch-aware
# Tarball expands straight into `./opencode` (no `bin/` folder).
# ============================================================================
RUN set -eux; \
    case "$TARGETARCH" in \
      amd64) OC_ASSET=opencode-linux-x64.tar.gz    ; OC_SHA="${OPENCODE_SHA256_amd64}" ;; \
      arm64) OC_ASSET=opencode-linux-arm64.tar.gz  ; OC_SHA="${OPENCODE_SHA256_arm64}" ;; \
      *) echo "TARGETARCH '$TARGETARCH' not supported" >&2; exit 1 ;; \
    esac; \
    curl -fsSLo /tmp/opencode.tar.gz \
      "https://github.com/anomalyco/opencode/releases/download/v${OPENCODE_VERSION}/${OC_ASSET}"; \
    echo "${OC_SHA}  /tmp/opencode.tar.gz" | sha256sum -c -; \
    tar -xzf /tmp/opencode.tar.gz -C /usr/local/bin/; \
    chmod +x /usr/local/bin/opencode; \
    rm -f /tmp/opencode.tar.gz

# ============================================================================
# Skills my-org/skills installed for the opencode agent (global scope).
# ============================================================================
RUN npx -y skills add my-org/skills \
      -a opencode \
      -s wiki-ingest \
      -s wiki-lint \
      -s wiki-init \
      -y \
      -g \
 && ls /root/.agents/skills/ \
 && echo "[build] skills installed"

# ============================================================================
# Trust extra CA (lab your-ca-issuer). Inert in prod.
# ============================================================================
# Optional: to trust a self-signed CA (internal registry/OIDC), place your
# ca.crt here and uncomment. Public registries/OIDC use the system bundle.
# COPY ca.crt /usr/local/share/ca-certificates/extra-ca.crt
# RUN update-ca-certificates 2>&1 | tail -2

# ============================================================================
# ETL scripts
# ============================================================================
COPY entrypoint.sh          /usr/local/bin/entrypoint.sh
COPY run-etl.sh             /usr/local/bin/run-etl.sh
COPY capacity-report.sh     /usr/local/bin/capacity-report.sh
COPY supercronic.tmpl       /etc/supercronic.tmpl
COPY ingest-instructions.md /usr/local/share/wiki-ingest/ingest-instructions.md

RUN chmod +x /usr/local/bin/entrypoint.sh /usr/local/bin/run-etl.sh /usr/local/bin/capacity-report.sh

# ============================================================================
# Default config
# ============================================================================
WORKDIR /data

# entrypoint.sh chooses between `cron` mode (default, exec supercronic as PID 1)
# and `run` (one-shot, exec run-etl.sh).
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD ["cron"]
