# scope-guard — admission webhook for ai-memory.
# Stdlib only (no go.sum). Static binary on scratch (~6 MB).
#
# Build:
#   docker build -t ghcr.io/djalmajr/ai-memory-ops/scope-guard:dev images/scope-guard

FROM golang:1.23-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/scope-guard .

FROM scratch
COPY --from=builder /out/scope-guard /scope-guard
EXPOSE 8080
# Numeric UID — Kubernetes `runAsNonRoot: true` rejects named users.
USER 65532:65532
ENV LISTEN_ADDR=0.0.0.0:8080
ENTRYPOINT ["/scope-guard"]
