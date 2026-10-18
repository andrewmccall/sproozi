# Build the Codex base with hack/demo/codex.Dockerfile from the repository root.
# Resolve and retain both source and resulting image digests before deployment.
ARG CODEX_CLIENTS_IMAGE
FROM ${CODEX_CLIENTS_IMAGE}
USER root
# Exact Claude Code from its committed lockfile, without install scripts.
COPY hack/clients/ /opt/sproozi-clients/
RUN sh /opt/sproozi-clients/install.sh claude-code
USER 1000:1000
