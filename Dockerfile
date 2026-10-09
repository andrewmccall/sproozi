# Build the manager binary
# The digest is the multi-platform manifest digest for the pinned Go release.
# Keeping the platform selector implicit lets BuildKit select the matching
# child manifest for TARGETARCH while retaining one auditable reference.
FROM golang:1.26-bookworm@sha256:e8c859f5632dcfde7b32d2012b4351728f6437930887c2f6a91ea242459e5514 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy release sources. Acceptance fixtures do not invalidate release builds.
COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/

# Build
# the GOARCH has no default value to allow the binary to be built according to the host where the command
# was called. For example, if we call make docker-build in a local env which has the Apple Silicon M1 SO
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -o manager cmd/main.go && \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -o gateway cmd/gateway/main.go && \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -o webhook cmd/webhook/main.go && \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -o tasks cmd/tasks/main.go

# The binaries are statically linked.  scratch removes a second mutable image
# reference; copy only the CA bundle needed for outbound TLS.
FROM scratch
WORKDIR /
COPY --from=builder /workspace/manager .
COPY --from=builder /workspace/gateway .
COPY --from=builder /workspace/webhook .
COPY --from=builder /workspace/tasks .
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
USER 65532:65532

ENTRYPOINT ["/manager"]
