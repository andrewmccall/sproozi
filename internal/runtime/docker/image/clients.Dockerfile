# Build the Codex base with hack/demo/codex.Dockerfile, the default client version.
# Resolve and retain both source and resulting image digests before deployment.
ARG CODEX_CLIENTS_IMAGE
FROM ${CODEX_CLIENTS_IMAGE}
USER root
RUN npm install --global @anthropic-ai/claude-code
USER 1000:1000
