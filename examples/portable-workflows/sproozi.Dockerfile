# Build from the repository root, with an administrator-resolved native image.
# NATIVE_IMAGE's default names the native-image-required stage, which fails the
# build with instructions, so a build without the argument stops with a clear error.
ARG NATIVE_IMAGE=native-image-required
FROM golang:1.26-bookworm@sha256:e8c859f5632dcfde7b32d2012b4351728f6437930887c2f6a91ea242459e5514 AS native-image-required
RUN echo "Set --build-arg NATIVE_IMAGE to the administrator-resolved native image: a local tag, or repository@sha256:digest for shared or Kubernetes builds" >&2 && exit 1

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
# The native supervisor and its entrypoint are inherited unchanged.
