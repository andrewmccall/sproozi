# Build clients with clients.Dockerfile and the pinned Codex package.
# CLIENTS_IMAGE must be the administrator-selected immutable client image.
# Its default names the clients-image-required stage, which fails the build
# with instructions, so a build without the argument stops with a clear error.
ARG CLIENTS_IMAGE=clients-image-required
FROM golang:1.26-bookworm@sha256:e8c859f5632dcfde7b32d2012b4351728f6437930887c2f6a91ea242459e5514 AS clients-image-required
RUN echo "Set --build-arg CLIENTS_IMAGE to the client image built with clients.Dockerfile: a local tag, or repository@sha256:digest for shared or Kubernetes builds" >&2 && exit 1

FROM golang:1.26-bookworm@sha256:e8c859f5632dcfde7b32d2012b4351728f6437930887c2f6a91ea242459e5514 AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -o /native-session ./cmd/native-session && \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -o /checkpoint ./cmd/checkpoint && \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -o /sproozi-network ./cmd/sproozi-network && \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -o /session-profile ./cmd/session-profile

FROM ${CLIENTS_IMAGE}
USER 0:0
# The trusted network anchor alone uses these binaries with NET_ADMIN. Native
# containers run UID1000 with no capabilities and cannot alter their rules.
RUN apt-get update && apt-get install -y --no-install-recommends iptables && \
    rm -rf /var/lib/apt/lists/* && \
    codex --version && \
    claude --version && git --version && \
    mkdir -p /workspace /home/agent && chown 1000:1000 /workspace /home/agent
COPY --from=builder /native-session /usr/local/bin/native-session
COPY --from=builder /checkpoint /usr/local/bin/checkpoint
COPY --from=builder /sproozi-network /usr/local/bin/sproozi-network
COPY --from=builder /session-profile /usr/local/bin/session-profile
USER 1000:1000
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/native-session"]
