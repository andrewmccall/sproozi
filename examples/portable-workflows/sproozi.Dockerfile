# Build from the repository root, with an administrator-resolved native image.
ARG NATIVE_IMAGE
FROM golang:1.26-bookworm@sha256:e8c859f5632dcfde7b32d2012b4351728f6437930887c2f6a91ea242459e5514 AS toolchain
ENV GOTOOLCHAIN=local GOMODCACHE=/opt/sproozi-go-modules CGO_ENABLED=0
WORKDIR /cache-source
COPY go.mod go.sum ./
RUN go mod download

FROM ${NATIVE_IMAGE}
USER 0:0
COPY --from=toolchain /usr/local/go /usr/local/go
COPY --from=toolchain /opt/sproozi-go-modules /opt/sproozi-go-modules
COPY examples/portable-workflows/go.sh /usr/local/bin/go
RUN chmod 0755 /usr/local/bin/go && \
    ln -s /usr/local/go/bin/gofmt /usr/local/bin/gofmt
USER 1000:1000
# The pinned native supervisor and its entrypoint are inherited unchanged.
