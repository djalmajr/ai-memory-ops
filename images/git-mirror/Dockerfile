# git-mirror — admission webhook that mirrors pages into a git repo.
# Needs `git` + `ssh-client` at runtime, so we land on alpine instead of
# scratch. Image size ~30 MB.
#
# Build:
#   docker build -t ghcr.io/djalmajr/ai-memory-ops/git-mirror:dev images/git-mirror

FROM golang:1.23-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/git-mirror .

FROM alpine:3.20
RUN apk add --no-cache git openssh-client ca-certificates \
 && adduser -D -u 65532 -h /work app \
 && mkdir -p /work/repo \
 && chown -R app:app /work
COPY --from=builder /out/git-mirror /usr/local/bin/git-mirror
USER 65532:65532
WORKDIR /work
EXPOSE 8080
ENV LISTEN_ADDR=0.0.0.0:8080
ENV WORK_DIR=/work/repo
ENTRYPOINT ["/usr/local/bin/git-mirror"]
