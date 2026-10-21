# Build the Codex base with hack/demo/codex.Dockerfile from the repository root.
# Resolve and retain both source and resulting image digests before deployment.
# The default names the codex-clients-image-required stage, which fails the
# build with instructions, so a build without the argument stops with a clear error.
ARG CODEX_CLIENTS_IMAGE=codex-clients-image-required
FROM golang:1.26.10-bookworm@sha256:632dff7351799a5a9954a48dc06670ade72ee44e8c351cdc0e5a57af2fea21af AS codex-clients-image-required
RUN echo "Set --build-arg CODEX_CLIENTS_IMAGE to the Codex base image built with hack/demo/codex.Dockerfile" >&2 && exit 1

FROM ${CODEX_CLIENTS_IMAGE}
USER root
# Exact Claude Code from its committed lockfile, without install scripts.
COPY hack/clients/ /opt/sproozi-clients/
RUN sh /opt/sproozi-clients/install.sh claude-code
USER 1000:1000
