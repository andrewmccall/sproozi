# Build the Codex base with hack/demo/codex.Dockerfile from the repository root.
# Resolve and retain both source and resulting image digests before deployment.
# The default names the codex-clients-image-required stage, which fails the
# build with instructions, so a build without the argument stops with a clear error.
ARG CODEX_CLIENTS_IMAGE=codex-clients-image-required
FROM golang:1.26.11-bookworm@sha256:dc40a13b61304647ed7e5d581d246b86413dfc29d63c6d018310feb93cd98fc0 AS codex-clients-image-required
RUN echo "Set --build-arg CODEX_CLIENTS_IMAGE to the Codex base image built with hack/demo/codex.Dockerfile" >&2 && exit 1

FROM ${CODEX_CLIENTS_IMAGE}
USER root
# Exact Claude Code and Copilot from their committed lockfiles, without
# install scripts.
COPY hack/clients/ /opt/sproozi-clients/
RUN sh /opt/sproozi-clients/install.sh claude-code copilot
USER 1000:1000
