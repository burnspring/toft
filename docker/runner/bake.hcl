# The five runner families, in one build.
#
# `scripts/k3s.sh runners` used to run five `docker build`s, one per family, and
# they queued: each one recomputed `runner-build` — the stage that copies all of
# `internal/` and compiles the runner binary — from a cache the previous family
# had just filled, and none of the five could use a core the one before it was
# not using. Bake declares them as one graph, so `runner-build` is solved once
# and the five families overlap on the box's eight cores.
#
# What this file deliberately does not decide is *which* families are built.
# `runner_stale` in scripts/k3s.sh decides that, per family, and hands the names
# here as bake targets; `verify_runner_declaration` then checks each image that
# came out, per family. The batching is of execution and never of the checks —
# an image that nobody compared to anything is the failure both of those exist
# to prevent.
#
# Build one family by hand exactly as the Dockerfile's header shows, or all five
# from the repository root with:
#
#   docker buildx bake -f docker/runner/bake.hcl
#
# TestTheBakeBuildsTheFamiliesTheScriptDeclares holds the target list and the
# assembly of each against scripts/k3s.sh.

# The tag's version, which scripts/k3s.sh passes from its own RUNNER_VERSION so
# the two cannot disagree about what `runner_image` named.
variable "RUNNER_VERSION" {
  default = "1"
}

group "default" {
  targets = ["python", "node", "go", "r", "static"]
}

# The day the build runs, so each runner's last apt-get upgrade re-runs daily
# and a Debian security fix reaches it without discarding the cache.
variable "APT_REFRESH" {
  default = "unset"
}

target "_common" {
  context    = "."
  dockerfile = "docker/runner/Dockerfile"
  args       = { APT_REFRESH = APT_REFRESH }
}

target "python" {
  inherits = ["_common"]
  args     = { FAMILY = "python", ASSEMBLY = "posix" }
  tags     = ["toft-runner-python:${RUNNER_VERSION}"]
}

target "node" {
  inherits = ["_common"]
  args     = { FAMILY = "node", ASSEMBLY = "posix" }
  tags     = ["toft-runner-node:${RUNNER_VERSION}"]
}

target "go" {
  inherits = ["_common"]
  args     = { FAMILY = "go", ASSEMBLY = "posix" }
  tags     = ["toft-runner-go:${RUNNER_VERSION}"]
}

target "r" {
  inherits = ["_common"]
  args     = { FAMILY = "r", ASSEMBLY = "posix" }
  tags     = ["toft-runner-r:${RUNNER_VERSION}"]
}

# The one family whose base has no shell, so its finish is a different assembly
# stage — see the assembly section at the bottom of docker/runner/Dockerfile.
target "static" {
  inherits = ["_common"]
  args     = { FAMILY = "static", ASSEMBLY = "static" }
  tags     = ["toft-runner-static:${RUNNER_VERSION}"]
}
