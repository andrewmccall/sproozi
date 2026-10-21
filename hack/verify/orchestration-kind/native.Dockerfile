# The managed-session acceptance image: the native supervisor, checkpoint
# helper and remote executor over the pinned stock Codex client. It mirrors
# internal/runtime/docker/image/Dockerfile but builds its own client layer, so
# a builder that cannot see local images (the CI buildx cache) can build it.
FROM golang:1.26.10-bookworm@sha256:632dff7351799a5a9954a48dc06670ade72ee44e8c351cdc0e5a57af2fea21af AS builder
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
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -o /session-profile ./cmd/session-profile

FROM node:22.16.0-bookworm-slim@sha256:048ed02c5fd52e86fda6fbd2f6a76cf0d4492fd6c6fee9e2c463ed5108da0e34
RUN apt-get update && apt-get install -y --no-install-recommends python3 ca-certificates curl git && \
    rm -rf /var/lib/apt/lists/*
COPY hack/clients/ /opt/sproozi-clients/
RUN sh /opt/sproozi-clients/install.sh codex && codex --version && git --version && \
    mkdir -p /workspace /home/agent && chown 1000:1000 /workspace /home/agent
COPY --from=builder /native-session /usr/local/bin/native-session
COPY --from=builder /checkpoint /usr/local/bin/checkpoint
COPY --from=builder /session-profile /usr/local/bin/session-profile
USER 1000:1000
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/native-session"]
