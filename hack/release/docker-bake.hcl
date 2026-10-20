// Release images, built from the same pinned recipes as local `--build`.
// Each target names its parent build as a context, so the existing
// Dockerfiles' FROM ${…_IMAGE} arguments resolve to the chained stage
// instead of a registry image.

variable "REPOSITORY" {
  default = "ghcr.io/andrewmccall"
}

variable "VERSION" {
  default = "dev"
}

group "default" {
  targets = ["native", "control"]
}

target "codex" {
  context    = "."
  dockerfile = "hack/demo/codex.Dockerfile"
}

target "clients" {
  context    = "."
  dockerfile = "internal/runtime/docker/image/clients.Dockerfile"
  contexts   = { codex = "target:codex" }
  args       = { CODEX_CLIENTS_IMAGE = "codex" }
}

// The native worker, checkpoint helper and network anchor image.
target "native" {
  context    = "."
  dockerfile = "internal/runtime/docker/image/Dockerfile"
  contexts   = { clients = "target:clients" }
  args       = { CLIENTS_IMAGE = "clients" }
  tags       = ["${REPOSITORY}/sproozi-native:${VERSION}"]
  labels     = { "org.opencontainers.image.source" = "https://github.com/andrewmccall/sproozi", "org.opencontainers.image.version" = "${VERSION}" }
}

// The controller, gateway, webhook and task service image.
target "control" {
  context    = "."
  dockerfile = "Dockerfile"
  tags       = ["${REPOSITORY}/sproozi:${VERSION}"]
  labels     = { "org.opencontainers.image.source" = "https://github.com/andrewmccall/sproozi", "org.opencontainers.image.version" = "${VERSION}" }
}
