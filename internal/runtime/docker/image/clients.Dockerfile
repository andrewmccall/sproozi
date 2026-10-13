# Build the Codex base with hack/demo/codex.Dockerfile, CODEX_VERSION=0.161.0.
# Resolve and retain both source and resulting image digests before deployment.
ARG CODEX_CLIENTS_IMAGE
FROM ${CODEX_CLIENTS_IMAGE}
USER root
ARG CLAUDE_VERSION=2.1.27
RUN npm install --global --ignore-scripts @anthropic-ai/claude-code@${CLAUDE_VERSION}
USER 1000:1000
