# The runner image: one per language family, never per app.
#
# Build with the family as an argument:
#
#   docker build -f docker/runner/Dockerfile --build-arg FAMILY=python \
#       -t toft-runner-python:dev .
#   scripts/k3s.sh load toft-runner-python:dev
#
# The static family needs a second argument, because its base has no shell to
# run the account setup in — see the assembly section at the bottom:
#
#   docker build -f docker/runner/Dockerfile --build-arg FAMILY=static \
#       --build-arg ASSEMBLY=static -t toft-runner-static:dev .
#
# Two properties matter and are easy to lose:
#
#   The image both builds and runs. Build mode installs dependencies and emits
#   a ready-to-run tree; run mode fetches that tree and execs it. Because they
#   are the same image, the build environment *is* the run environment — which
#   is what lets a venv built here actually work here, and what makes an app
#   movable between shared and isolated mode without a rebuild.
#
#   It carries a language runtime and the runner binary, and nothing else of
#   Toft's. The platform and the runner deploy independently and are held
#   together by the versioned callback contract, not by shipping together.

# FAMILY selects the base image below. It is declared here, before the first
# FROM, because only a global ARG may be referenced in a FROM line.
ARG FAMILY=python
# ASSEMBLY selects how the image is finished: `posix` for a base with a shell,
# `static` for one without. Two values rather than a conditional RUN, because a
# RUN in an image with no /bin/sh does not fail informatively — it fails at
# `docker build` with a message about the wrong thing.
ARG ASSEMBLY=posix

# --- runner binary -----------------------------------------------------------
# -trixie for currency, not for correctness. This stage builds CGO_ENABLED=0 (see
# below), so its output carries no glibc coupling in either direction and it was
# free to stay where it was; it moved because bookworm left Debian's regular
# security support on 2026-07-11. It is deliberately *not* what
# TestAGoBinaryIsBuiltAndRunOnTheSameDebian is about — that test is family-go's.
FROM golang:1.27-trixie AS runner-build
WORKDIR /src

# Dependencies first, so a change to our own source does not re-download them.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY cmd/ ./cmd/
COPY internal/ ./internal/
# The app SDKs are embedded into the binary, because the runner installs them
# into an app's environment during a build and an on-prem install has no reason
# to reach PyPI or npm for a library that ships in this same release.
COPY sdk/ ./sdk/

# Static, so one binary drops into any base image regardless of its libc. It is
# what makes the static family's empty base possible, and it was already built
# this way before that family existed.
# This stage is where a runner pass spends its Go time, and it is invalidated by
# most commits: it copies all of internal/, so any change to the platform's own
# packages recompiles it. Cached, that becomes an incremental compile — and one
# incremental compile for all five families rather than five cold ones, since
# BuildKit builds this stage once and the families depend on it.
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build,id=gobuild-runner-$TARGETARCH \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/toft-runner ./cmd/toft-runner

# An empty directory to hand the static assembly, which has no `mkdir` to run.
RUN mkdir -p /skel/workspace /skel/home

# --- python ------------------------------------------------------------------
# 3.15, 3.14 and 3.13 — **the same three the platform image carries**, which the
# root Dockerfile's stage 3 argues for and this file does not repeat. The two
# sets move together or an app gets a different Python depending on which
# execution mode it landed in, which is what 3.12-here-3.11-there already was
# and what a set matching in one entry and not the others would quietly become.
#
# **This set is declared to the platform twice**, and both are checked. As
# RUNNER_PYTHON_VERSION (the chart's `runnerVersions.python`, and scripts/k3s.sh
# for this box), which is what a `[runtime]` pin is judged against for an
# isolated app, since the build happens on the platform host before any pod
# exists — and the runner probes its own image at start and refuses a workload
# that declaration got wrong, naming *every* series that is missing rather than
# the first (ADR §64). And, since the platform's own interpreters became paths
# in a file rather than a property of its Debian, against those:
# TestThePlatformImageDeclaresTheSamePythonAsTheRunner fails in
# scripts/check.sh if these two images stop agreeing, which is the guard this
# row went without for as long as there was nothing to compare it to.
FROM python:3.15-slim-trixie AS family-python
# uv is what the shared backend uses for venv creation and installs; using the
# same tool in both places is what keeps a build reproducible across modes —
# which this line claimed while pinning 0.5.11 against the platform image's
# 0.12.5, twenty months apart, so the claim was false for as long as it stood.
# The two pins must be the same string and
# TestUvIsPinnedToTheSameVersionInBothImages fails the build when they are not.
COPY --from=ghcr.io/astral-sh/uv:0.13.6@sha256:af22a52f5eafef3d78e551599d405c02df61919d18028f0cea8eda93bc9986d0 /uv /usr/local/bin/uv

# The series this stage is declared to carry, first entry first — the Python peer
# of family-r's TOFT_R_SERIES, and read by the same k3s.sh guard.
#
# The first is the base image's own, at /usr/local/bin/python3, which is the
# interpreter on PATH and what an app with no pin gets. It is *not* repeated in
# TOFT_PYTHON_PATH below: the probe finds it by name.
ENV TOFT_PYTHON_SERIES=3.15,3.14,3.13

# The older Pythons, from uv, **at the same paths the platform image puts
# them** — and that identity is load-bearing rather than tidy.
#
# A venv records the absolute path of the interpreter it was built against, and
# every release is built on the platform host in both execution modes. When the
# path exists here the tree arrives usable and `repairRelease` does nothing;
# when it does not, the pod discards the venv and reinstalls every dependency
# before the app can start. That repair is what an isolated Python app has always
# done, because this image's own 3.15 lives at /usr/local/bin/python3 and the
# platform's at /opt/python/3.15 — and it is what an app resolving to 3.14 or
# 3.13 now skips. The asymmetry is worth knowing about and is not worth closing
# by moving the base image's Python: a repair is slow, not wrong.
#
# uv binds a venv to the *minor* symlink rather than the patch directory, which
# is what makes the two images' paths agree without their patch levels having
# to: rebuild one of them a month later, get 3.12.12 against 3.12.11, and
# nothing already built dangles.
#
# The argument for carrying them at all — a pin is refused and never
# approximated, and a wheel built for cp313 does not load on 3.15 — is in the
# root Dockerfile beside its copy of this install, and is not repeated here. So is
# the reason every pip here, the base image's own included, is upgraded to a floor.
RUN uv python install --install-dir /opt/python 3.14 3.13 && \
    for v in 3.14 3.13; do \
      mkdir -p /opt/python/$v/bin && \
      ln -s /opt/python/cpython-$v-*/bin/python3 /opt/python/$v/bin/python3 && \
      /opt/python/$v/bin/python3 -V && \
      /opt/python/$v/bin/python3 -m pip install --no-cache-dir --break-system-packages \
        --root-user-action=ignore --upgrade 'pip>=26.2.2'; \
    done && \
    python3 -m pip install --no-cache-dir --root-user-action=ignore --upgrade 'pip>=26.2.2' && \
    got=$(python3 -c 'import sys; print("%d.%d" % sys.version_info[:2])') && \
    want=${TOFT_PYTHON_SERIES%%,*} && \
    [ "$got" = "$want" ] || \
    { echo "family-python declares Python $want first but this image's own is $got"; exit 1; }

# Where the declared series are, in the form the platform's probe reads. The
# declaration above is what an app is *judged* against and this is what each one
# is *run* from; the probe execs every entry and believes what it answers over
# either string, which is why a typo here fails the runner's boot rather than
# becoming a mismatched interpreter three weeks later.
ENV TOFT_PYTHON_PATH=/opt/python/3.14/bin/python3:/opt/python/3.13/bin/python3

# --- node --------------------------------------------------------------------
# Kept on the same majors as the platform image's runtime stage (the root
# Dockerfile's stage 3), which is where the reasoning for *which* majors is
# written down — 26 because it is the active LTS, 24 and 22 because a native
# module is compiled against one major's ABI and an app that has one is pinned
# to the major it was written for. The two sets move together or an app gets a
# different Node depending on which execution mode it landed in. Declared to the
# platform as RUNNER_NODE_VERSION, on the same terms as family-python above.
FROM node:26-trixie-slim AS family-node
# The npm the Node image bundles lags the registry by however long it has been
# since that image was last pushed, and npm is not a bystander here — it is what
# runs against a deployed app's package.json, so its own vendored tar and undici
# are on the path that handles repository content. Left alone this image ships a
# CRITICAL in node-tar that npm fixed weeks ago. Same upgrade, same reason, as
# the platform image's runtime stage, and the same floor (`^12.2.1`), for the
# reason written beside it there.
RUN npm install -g 'npm@^12.2.1' && npm cache clean --force


# node:26-trixie-slim ships no CA bundle at all — /etc/ssl/certs is empty and
# ca-certificates is not installed — which the other three families do not share
# (python-slim, golang and distroless-static all carry one). Nothing in a Node
# *app* necessarily notices, but the runner does: its first act in run mode is to
# fetch the release over HTTPS from the artifact store, and Go reads the system
# pool. Without this a Node workload dies at artifact_fetch with
# `x509: certificate signed by unknown authority` against a presigned S3 URL —
# an error that reads as a broken signature or a clock skew rather than an empty
# trust store. TestEveryRunnerFamilyTrustsPublicTLS is what keeps the four
# families honest about this.
# apt's two caches are mounts rather than layers, and the `rm -rf
# /var/lib/apt/lists/*` that used to end every apt instruction here is gone with
# them. The size argument that put it there is not being reversed, it is being
# satisfied better: a cache mount never lands in a layer at all, whereas the
# deletion only removed the lists from the *final* layer after they had already
# been written into an intermediate one. Debian's docker-clean is removed in the
# same instruction, in every one of them, because it deletes the archive apt was
# just handed and a mount without it silently buys nothing.
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    rm -f /etc/apt/apt.conf.d/docker-clean && \
    apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates

# The series this stage carries, base image's first. The Node peer of
# family-python's declaration above and family-r's below.
ENV TOFT_NODE_SERIES=26,24,22

# The older LTS lines (24 and 22), from nodejs.org's tarballs, exactly as the
# platform image installs them — the argument for the pair, and for the 366 MB is written out beside that copy and not repeated.
#
# What differs at this end is what the coupling is *for*. A Python venv is bound
# to an interpreter path, so the two images' paths have to agree; `node_modules`
# is bound to nothing and is portable between machines. What is not portable is
# a **native module**, compiled against one major's ABI on the platform host and
# loaded by `require` here — so the two images have to carry the same majors for
# the reason family-r and the platform image have to carry the same R: the
# artifact is built there and executed here.
#
# The `<version>:<npm tag>` pairs are the same statement the platform image
# makes and for the same reason.
ARG TARGETARCH
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    set -eux; \
    rm -f /etc/apt/apt.conf.d/docker-clean; \
    case "$TARGETARCH" in \
      amd64) arch=x64 ;; \
      arm64) arch=arm64 ;; \
      *) echo "no Node build for $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl; \
    # version:npm:x64-sha256:arm64-sha256 — the same hashes as the root
    # Dockerfile's (TestBothImagesVerifyTheSameToolchainDownloads).
    for entry in \
      24.22.2:latest:361718c444e49af2ca5a110c83ba6a8901143f23ab285ebbef0341f391f482a9:95c264150aceb7f3437a29fd6984924f5f8d1511ea3cbd6c508b83f6c555dae2 \
      22.23.4:latest:b8492284c76a54baf58d61668dd0e41ee0fc98adabf9503d30f05d08f55b4406:f7a1e486dbc8b286847815b477d4fb5e9fb963ed4489c3e1588853d685f589f9; do \
      v=${entry%%:*}; rest=${entry#*:}; npm_tag=${rest%%:*}; sums=${rest#*:}; major=${v%%.*}; \
      case "$arch" in x64) sum=${sums%%:*} ;; arm64) sum=${sums#*:} ;; esac; \
      case ",${TOFT_NODE_SERIES}," in \
        *",${major},"*) ;; \
        *) echo "family-node installs Node ${major}, which TOFT_NODE_SERIES=${TOFT_NODE_SERIES} does not name"; exit 1 ;; \
      esac; \
      mkdir -p "/opt/node/$major"; \
      curl -fsSL -o /tmp/node.tar.gz "https://nodejs.org/dist/v${v}/node-v${v}-linux-${arch}.tar.gz"; \
      echo "$sum  /tmp/node.tar.gz" | sha256sum -c -; \
      tar xzf /tmp/node.tar.gz -C "/opt/node/$major" --strip-components=1 \
            --exclude='*/share/doc' --exclude='*/share/systemtap' --exclude='*/CHANGELOG.md'; \
      rm -f /tmp/node.tar.gz; \
      "/opt/node/$major/bin/node" -v; \
      PATH="/opt/node/$major/bin:$PATH" "/opt/node/$major/bin/npm" install -g "npm@${npm_tag}"; \
      "/opt/node/$major/bin/npm" cache clean --force; \
    done; \
    apt-get purge -y --auto-remove curl; \
    got=$(node -p 'process.versions.node.split(".")[0]'); \
    want=${TOFT_NODE_SERIES%%,*}; \
    [ "$got" = "$want" ] || \
    { echo "family-node declares Node $want first but this image's own is $got"; exit 1; }
ENV TOFT_NODE_PATH=/opt/node/24/bin/node:/opt/node/22/bin/node

# --- go ----------------------------------------------------------------------
# Go apps compile to a binary at build time, but the toolchain has to be
# present in build mode, so the family image carries it.
#
# **-trixie, and the suffix is the whole point of this line.** A Go app's binary
# is never built in this image: `go build` runs on the platform host, which is
# the root Dockerfile's runtime stage — a node:26-trixie-slim filesystem with
# /usr/local/go copied into it — and the pod execs the result. So the two ends of
# a binary are that stage and this one, and while this said bookworm they were
# glibc 2.41 and 2.36. A binary linked against the newer does not run on the
# older: it dies at exec with a missing symbol, in the pod, with nothing about
# Debian in the message.
#
# It could not happen while it stood, which is why nobody hit it — the platform
# image carries no C compiler, so a package importing "C" fails at `go build` and
# no binary is produced, and pure-Go binaries are static and do not care. That
# made it a trap that would arm itself the day somebody added build-essential for
# a native npm module, surfacing in a language family other than the one that
# changed. The inversion worth remembering: family-go has gcc and had the older
# glibc, and the image that does the compiling has neither.
#
# The build end is not free to move — it is a Node image, on trixie for the
# Python that Debian release carries — so this is the only end with a degree of
# freedom, and spending it also closes a base that left regular security support
# on 2026-07-11. TestAGoBinaryIsBuiltAndRunOnTheSameDebian holds the pair.
FROM golang:1.27-trixie AS family-go

# --- r -----------------------------------------------------------------------
# **debian:trixie-slim + Posit's R for debian-13, and both halves of that are
# the whole decision.** A binary R package is compiled against a distribution's system
# libraries, and the build that produces an renv library runs on the *platform*
# host in both execution modes — the root Dockerfile's runtime stage, which is a
# trixie filesystem. So the two ends of an renv library are that stage and this
# one, and they have to be the same Debian for the same reason family-go and the
# platform image have to be: a mismatch does not fail the build, it unpacks
# cleanly and then dies at dyn.load in the pod, one package at a time, with
# nothing about Debian in the message.
#
# R came from apt until 4.6, which kept the pair honest by construction; trixie's
# apt stops at 4.5, so every R here is now Posit's build for the same trixie, at
# the same patch strings as the platform image's, held equal by
# TestBothImagesCarryTheSameRSeries. What is declared to the platform is the
# *series* (RUNNER_R_VERSION=4.6,4.5,4.4), which is the unit a pin is judged at —
# patch never matters.
#
# **No compiler is ever used here, on purpose.** Posit's R debs depend on gcc,
# gfortran and make, so the toolchain is on disk, but nothing in a pod runs it:
# the renv library is restored on the platform host and arrives built. Adding
# r-base-dev or build-essential beside it would be a decision to compile in a
# pod, and refusing that is the bargain
# TOFT_R_REPOS exists to make: a first deploy of anything touching the
# tidyverse is twenty to forty minutes of gcc and gfortran, inside a compiled-in
# ten-minute command timeout, on a queue whose default width is one. An install
# with no route to a binary repository cannot deploy R — stated in
# config.DefaultRRepos rather than discovered here.
#
# ca-certificates for the reason family-node needs it: the runner's first act in
# run mode is to fetch the release over HTTPS, and TestEveryRunnerFamilyTrustsPublicTLS
# is what keeps the families honest about it.
# The older R series, downloaded where no curl has to survive into the image.
#
# family-r ships no curl — it fetches nothing at runtime, because the runner
# binary is Go and speaks HTTPS itself — and adding one so that a build step
# could use it would leave a tool in every pod for the sake of a step that ran
# once. A stage that is thrown away is the cheaper shape, and it is the one this
# file already reaches for elsewhere.
#
# **Posit's builds, for debian-13.** The argument for the pair, the 123 MB and
# the reason a build for any other distribution would be worse than none is
# written out in the root Dockerfile beside its copy of this download, and is
# not repeated here. What matters at this end is narrower and is the reason this
# stage exists at all: the platform *restores* an renv library against one of
# these interpreters and this image *loads* it, so the two ends have to be the
# same R and the same Debian, exactly as they have to be for the default.
FROM debian:trixie-slim AS r-debs
ARG TARGETARCH
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    rm -f /etc/apt/apt.conf.d/docker-clean && \
    apt-get update && \
    apt-get install -y --no-install-recommends curl ca-certificates && \
    mkdir -p /debs && \
    for entry in \
      4.6.2:e8fe0cace604d1b44515522cde5440a0937301f2660f282f908cb1e0deef15e8:1c4992e23c48a6846777b12b221e6333efb6c011e68ef88b797dc5512969e113 \
      4.5.3:5fd9f26826a14b4a73c17aa6656ad3ee52578acddcb896a76816af8ef604c5b6:864b0d0a1045d181fa7790c2a2352196bd37d1be993f7eea9cd1efe7086d07e5 \
      4.4.3:d8b8ea1d3169ad840d15c3cd276cd9fbc250cad1ac36e1a58f8798e63e592169:82d2b2bdd6e5f3443b06176f82a9ea8386759f0afb96a23e077dba46232c18e9; do \
      v=${entry%%:*}; sums=${entry#*:}; \
      case "$TARGETARCH" in amd64) sum=${sums%%:*} ;; arm64) sum=${sums#*:} ;; *) echo "no R build for $TARGETARCH" >&2; exit 1 ;; esac; \
      curl -fsSL -o "/debs/r-${v}.deb" \
        "https://cdn.posit.co/r/debian-13/pkgs/r-${v}_1_${TARGETARCH}.deb"; \
      echo "$sum  /debs/r-${v}.deb" | sha256sum -c - || exit 1; \
    done

FROM debian:trixie-slim AS family-r
# The series this stage is declared to carry, and the build refuses to produce
# an image that disagrees with it. There is no version in the FROM tag to read —
# R arrives as Posit's debs — so without this line RUNNER_R_VERSION would be a claim
# about a stage with nothing in it to check, which is the shape the Python row
# of the toolchain table had for as long as the platform's interpreter was
# "whatever Debian carries". Here the declaration is a string in this file, the
# RUNs below hold it against the interpreters actually installed, and
# TestTheDeclaredRunnerRMatchesTheImage holds scripts/k3s.sh against the string.
# A deb that installs a series this line does not name fails the build rather
# than the cluster.
#
# **A list, and the first entry is the newest** (ADR §64 D19). An image may carry
# several R series and an isolated app has to be able to resolve to the one its
# lockfile names — the alternative is what this platform did until now, which
# was to judge every isolated R app against one series and let a Connect bundle
# pinned to an older R compile from source against headers it predates. The
# order is the contract toolchain.Configured reads: first is the default, the
# interpreter on PATH, the one an app with no declaration gets.
ENV TOFT_R_SERIES=4.6,4.5,4.4
#
# The system libraries, for the reason the platform stage installs them — the
# argument, the eight-name list and the 136 MB are written out there and not
# repeated here. **The two lists have to stay equal**: the release's library is
# restored on the platform and *loaded* here, so a library present at restore
# and absent at load produces a release that unpacks cleanly and then fails at
# dyn.load inside the pod — the same shape as naming another distribution's
# binaries, and just as invisible until a viewer gets a 502. Neither image's
# build fails when they drift, because each is individually correct and only an
# R app notices, so TestBothImagesInstallTheSameRSystemLibraries is what holds
# them together.
ARG R_SYSTEM_LIBRARIES="libuv1t64 libsodium23 \
    libgdal36 libproj25 libgeos-c1t64 libudunits2-0 libgit2-1.9 libabsl20240722 \
    libglpk40 libxml2 libwebpmux3 libfreetype6 libpoppler-cpp2 libnode115 libnetcdf22"
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    rm -f /etc/apt/apt.conf.d/docker-clean && \
    apt-get update && apt-get upgrade -y && \
    apt-get install -y --no-install-recommends ca-certificates $R_SYSTEM_LIBRARIES

# The three series, installed from the debs the throwaway stage fetched.
#
# They land in /opt/R/<version>, and the newest is linked into /usr/local/bin as
# R and Rscript, which makes it the interpreter on PATH and the default this
# image is declared to have. The check holds every deb against the declaration
# for the reason it always has: the declaration above is
# what a `[runtime]` pin and a lockfile's R.Version are judged against on the
# platform, so a stage that quietly carried something else would be a refusal
# the *runner* issues at start, on an app whose deploy succeeded.
#
# Every series in TOFT_R_SERIES after the first has to be here, and every
# one here has to be in it — walked in both directions, because the two failures
# are different and only one of them is loud. A declared series the image lacks
# is an app that deploys and then will not start; an installed series nobody
# declared is invisible, and it is dead weight in every pod.
COPY --from=r-debs /debs /tmp/debs
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    set -eux; \
    rm -f /etc/apt/apt.conf.d/docker-clean; \
    apt-get update; \
    apt-get install -y --no-install-recommends /tmp/debs/*.deb; \
    rm -rf /tmp/debs; \
    ln -s /opt/R/4.6.2/bin/R /usr/local/bin/R; \
    ln -s /opt/R/4.6.2/bin/Rscript /usr/local/bin/Rscript; \
    want=${TOFT_R_SERIES%%,*}; \
    got=$(Rscript -e 'cat(R.version$major, ".", sub("[.].*", "", R.version$minor), sep="")'); \
    [ "$got" = "$want" ] || { echo "family-r declares R $want first but its default is $got"; exit 1; }; \
    for v in 4.6.2 4.5.3 4.4.3; do \
      s=$("/opt/R/${v}/bin/Rscript" -e 'cat(R.version$major, ".", sub("[.].*", "", R.version$minor), sep="")'); \
      case ",${TOFT_R_SERIES}," in \
        *",${s},"*) ;; \
        *) echo "family-r installed R ${s} at /opt/R/${v}, which TOFT_R_SERIES=${TOFT_R_SERIES} does not name"; exit 1 ;; \
      esac; \
    done; \
    echo "${TOFT_R_SERIES}" | tr ',' '\n' | while read -r s; do \
      [ -n "$s" ] || continue; \
      command -v Rscript >/dev/null && \
        [ "$s" = "$(Rscript -e 'cat(R.version$major, ".", sub("[.].*", "", R.version$minor), sep="")')" ] && continue; \
      ls -d /opt/R/${s}.* >/dev/null 2>&1 || \
        { echo "family-r declares R ${s} and carries no such interpreter"; exit 1; }; \
    done

# Where they are, in the form the platform's probe reads. The declaration above
# is the *series* an app is judged against and this is the *path* each one is
# run from; toolchain probes every entry and believes what it answers over
# either string, which is why a typo here fails the runner's boot rather than
# becoming a mismatched library three weeks later.
ENV TOFT_R_PATH=/opt/R/4.6.2/bin/Rscript:/opt/R/4.5.3/bin/Rscript:/opt/R/4.4.3/bin/Rscript

# openssl, because the R SDK's caller_role verifies an HMAC and R has none in
# base. It is on its own RUN rather than on the system-library line
# deliberately: that line is held equal to the platform image's by
# TestBothImagesInstallTheSameRSystemLibraries because a binary R package links
# what the distribution has, and this is an R package rather than a library an
# renv restore links against.
#
# **Into every R's .Library**, because an app runs under the series its lockfile
# names rather than the default, and because both launchers set R_LIBS_SITE to
# the *release's* library — so a package anywhere else is a package the app
# cannot see, and the SDK would fail to load inside the one deployment it exists
# for. From the P3M snapshot config.DefaultRRepos names, under the User-Agent
# internal/runner/rlaunch.go sends, since P3M answers an unmodified R with
# source. P3M builds trixie binaries for x86_64 only, so an arm64 build
# compiles openssl from source — which works without naming anything here,
# because Posit's R deb depends on libcurl4-openssl-dev and that on libssl-dev.
# **Do not purge it afterwards**: apt would take R with it, down that chain.
#
# The check runs with R_LIBS_SITE pointed somewhere that does not exist, which
# is the condition a deployed app actually runs under. Without it this stage
# would ship a package that imports cleanly in a `docker run` and is missing in
# a pod, which is the exact shape of failure the R family already paid for once.
#
# **Then every package's `doc/` folder goes**, in every R: vignettes and their
# rendered HTML, which nothing in a running app reads. One of them is `openssl`'s
# own `keys.html`, which prints two example private keys — one under the password
# "supersecret" — so a secret scanner reported this image as shipping six private
# keys, one pair per R. Removing the folder is the honest fix and a smaller
# image; a waiver would be the wrong answer to a secret finding (scan-image.sh).
# `help/` and `html/` stay: `?topic` and `help()` read them.
RUN set -eu; for r in /opt/R/*/bin/Rscript; do \
      "$r" -e 'options(HTTPUserAgent = sprintf("R/%s R (%s)", getRversion(), paste(getRversion(), R.version$platform, R.version$arch, R.version$os))); install.packages("openssl", lib = .Library, repos = "https://p3m.dev/cran/__linux__/trixie/latest")'; \
      R_LIBS_SITE=/nonexistent "$r" -e 'if (!requireNamespace("openssl", quietly = TRUE) || length(openssl::sha256(charToRaw("x"), key = charToRaw("k"))) != 32L) { cat("family-r cannot compute an HMAC where a deployed app runs, under", R.version.string, "\n"); quit(status = 1) }'; \
      rm -rf "${r%/bin/Rscript}"/lib/R/library/*/doc; \
    done

# pandoc, for the one R workload that renders where it runs: a Shiny document
# (an .Rmd with `runtime: shiny`) is served by `rmarkdown::run`, which renders
# the document at its first request, inside the pod — and rmarkdown refuses
# without pandoc. Every other R document is rendered once at build time on the
# platform and ships as a directory, which is why this stage carried none until
# Shiny documents were started properly (ADR §64, third 2026-11-03 black-box
# list, BB-002). 189 MB, the price of an isolated install serving what a shared
# one does.
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    rm -f /etc/apt/apt.conf.d/docker-clean && \
    apt-get update && \
    apt-get install -y --no-install-recommends pandoc && \
    pandoc --version | head -1

# --- static --------------------------------------------------------------------
# The smallest thing in the estate, and deliberately not one of the three above.
#
# Borrowing the Go family image is the intuition everybody has and it is wrong:
# the Go *binary* is small, but the Go family *image* is the largest published
# here (~840 MB against python's 165 and node's 234), because it both builds and
# runs and so has to carry the whole toolchain. This one is 9.7 MB.
#
# An empty base is sufficient because a static workload does four things and
# none of them needs an operating system: nothing is installed (buildSteps
# returns no steps), nothing is repaired (there is no venv and no node_modules
# for `unrunnable` to miss), the release fetch is pure Go (archive/tar, never a
# shell out to tar or curl), and the pod identifies its user numerically.
#
# distroless/static rather than scratch, for its CA bundle — the release is
# fetched over HTTPS from a presigned URL — and its /etc/passwd and /tmp.
#
# The absence of a shell is a feature with one consequence, and it is the
# correct outcome rather than a limitation to work around: a job's command runs
# as `/bin/sh -c`, and a static app has no runtime in which a command could mean
# anything, so a toft.toml declaring jobs on a static app fails the deploy
# (internal/builder/manifest.go). Scheduled tasks are unaffected — those are
# HTTP requests to a path the app published, which a file server answers like
# any other.
# -debian13, matching every other base here. Almost all of these 9.7 MB *is*
# Debian — the CA bundle the release fetch needs, /etc/passwd, /tmp — so "there
# is barely anything in it" is an argument for the bump being cheap, not for
# skipping it.
FROM gcr.io/distroless/static-debian13:latest AS family-static

# --- assembly ------------------------------------------------------------------
# Two finishes, because one of the bases has no shell. They must agree on the
# UID, the workdir, the binary's path and the entrypoint; only the mechanism for
# creating the directories differs.

FROM family-${FAMILY} AS assembly-posix

# Security updates, refreshed once a day — the same layer and the same reason as
# the platform image's (Dockerfile, APT_REFRESH): each family's own upgrade is a
# cached layer, so a Debian fix published after it never reaches a rebuilt
# runner. Here, after the family, every posix runner gets it in one place;
# static is distroless and has no package manager to ask.
ARG APT_REFRESH=unset
RUN echo "security updates as of ${APT_REFRESH}" \
    && apt-get update && apt-get upgrade -y \
    && rm -rf /var/lib/apt/lists/*

# Non-root by default. An isolated workload has its own network namespace and
# its own resource envelope, but running as root inside it would still hand a
# compromised app the container's full capability set for free.
RUN groupadd --system --gid 10001 toft \
    && useradd --system --uid 10001 --gid toft --create-home --home-dir /home/toft toft \
    && mkdir -p /workspace \
    && chown -R toft:toft /workspace

COPY --from=runner-build /out/toft-runner /usr/local/bin/toft-runner

# Numeric, not the account name. Kubernetes verifies RunAsNonRoot by UID and
# refuses a container whose image names a user it cannot resolve to a number —
# a name here fails at container creation with no useful message.
USER 10001:10001
WORKDIR /workspace
ENV HOME=/home/toft \
    TOFT_WORK_DIR=/workspace
ENTRYPOINT ["/usr/local/bin/toft-runner"]

FROM family-static AS assembly-static

# There is no `mkdir` and no `chown` to be had, so the directories arrive as
# copies of empty ones with their ownership set on the way in. No `useradd`
# either — which costs nothing, because Kubernetes resolves the user by UID and
# the line below was already numeric for that reason.
COPY --from=runner-build --chown=10001:10001 /skel/workspace /workspace
COPY --from=runner-build --chown=10001:10001 /skel/home /home/toft
COPY --from=runner-build /out/toft-runner /usr/local/bin/toft-runner

USER 10001:10001
WORKDIR /workspace
ENV HOME=/home/toft \
    TOFT_WORK_DIR=/workspace
ENTRYPOINT ["/usr/local/bin/toft-runner"]

# No CMD arguments: the mode and every input arrive through the environment,
# and everything sized arrives in the bootstrap *response* rather than in a
# task override — ECS caps overrides at about 8KB, and secrets in an override
# would be readable from DescribeTasks output.
FROM assembly-${ASSEMBLY} AS final
