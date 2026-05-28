# Multi-stage build for contributors-webhook (ai-memory admission hook).
# Final image is distroless/static (~3-5 MB).

# =============================================================================
# Stage 1: Build (Rust)
# =============================================================================
FROM rust:1.86-slim-bookworm AS builder

WORKDIR /build

# Cache deps first.
COPY Cargo.toml ./
RUN mkdir -p src && echo "fn main(){}" > src/main.rs && \
    cargo build --release && \
    rm -rf src

# Real source.
COPY src ./src
RUN touch src/main.rs && \
    cargo build --release && \
    strip target/release/contributors-webhook

# =============================================================================
# Stage 2: Runtime (distroless static)
# =============================================================================
FROM gcr.io/distroless/cc-debian12:nonroot

WORKDIR /app
COPY --from=builder /build/target/release/contributors-webhook /usr/local/bin/contributors-webhook

USER nonroot
EXPOSE 8080
ENV LISTEN_ADDR=0.0.0.0:8080
ENV RUST_LOG=info

ENTRYPOINT ["/usr/local/bin/contributors-webhook"]
