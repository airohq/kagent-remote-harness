# The kagent harness (target harness) and the runner (target runner), from
# one build stage.
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags='-s -w' -o /out/harness ./cmd/harness \
    && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o /out/remote-harness ./cmd/remote-harness \
    && mkdir -p /out/data /out/work/workspace /out/work/state

# The runner on your machine: remote-harness with Claude Code, Codex, git and
# gh. It runs as user 10001 and keeps everything it writes under the /work
# volume, so it works with --read-only and --tmpfs /tmp.
FROM node:22-bookworm-slim AS runner
ARG CLAUDE_CODE_VERSION=2.1.323
ARG CODEX_VERSION=0.168.0
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git gh openssh-client ripgrep \
    && rm -rf /var/lib/apt/lists/* \
    && npm install -g "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" "@openai/codex@${CODEX_VERSION}" \
    && npm cache clean --force \
    && useradd --uid 10001 --user-group --no-create-home --home-dir /tmp/home runner
COPY --from=build /out/remote-harness /usr/local/bin/remote-harness
COPY --from=build --chown=10001:10001 /out/work /work
ENV HOME=/tmp/home \
    REMOTE_HARNESS_WORKDIR=/work/workspace \
    REMOTE_HARNESS_STATE_DIR=/work/state \
    CLAUDE_CONFIG_DIR=/work/state/claude \
    CODEX_HOME=/work/state/codex \
    DISABLE_AUTOUPDATER=1
USER 10001:10001
WORKDIR /work/workspace
VOLUME /work
EXPOSE 8080
ENTRYPOINT ["remote-harness"]
CMD ["serve", "--help"]

# The kagent BYO harness. kagent connects to it on port 80, and the probe on
# 8081, as a user without root, like kagent's own harnesses.
FROM gcr.io/distroless/static:nonroot AS harness
COPY --from=build /out/harness /harness
# kagent mounts the durable /data volume here and keeps the bootstrap bundle
# and the continuation in it.
COPY --from=build --chown=65532:65532 /out/data /data
EXPOSE 80 8081
ENTRYPOINT ["/harness"]
