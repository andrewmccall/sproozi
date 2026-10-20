# Build the Codex base with hack/demo/codex.Dockerfile from the repository root.
# Resolve and retain both source and resulting image digests before deployment.
# The default names the codex-clients-image-required stage, which fails the
# build with instructions, so a build without the argument stops with a clear error.
ARG CODEX_CLIENTS_IMAGE=codex-clients-image-required
FROM golang:1.26-bookworm@sha256:e8c859f5632dcfde7b32d2012b4351728f6437930887c2f6a91ea242459e5514 AS codex-clients-image-required
RUN echo "Set --build-arg CODEX_CLIENTS_IMAGE to the Codex base image built with hack/demo/codex.Dockerfile" >&2 && exit 1

FROM ${CODEX_CLIENTS_IMAGE}
USER root
# Exact Claude Code from its committed lockfile, without install scripts.
COPY hack/clients/ /opt/sproozi-clients/
RUN sh /opt/sproozi-clients/install.sh claude-code
USER 1000:1000
