# Toft container image.
#
# Toft supervises deployed apps as OS processes inside this container —
# but NOT as the same user. Each app runs as its own auto-created system
# account, its release directory is chowned to it and made 0700, so apps cannot
# read the platform's files or each other's.
#
# That posture is not set here any more, and the absence is deliberate: the
# platform derives it from its own uid — root, running shared workloads — so an image that
# stated it would be a fourth restatement of what the code already knows. Set
# APP_USER_PER_APP=0 to turn it off.
#
# On AWS, also set BLOCK_AWS_METADATA=1 and grant CAP_NET_ADMIN so app
# processes cannot read the task/instance IAM credentials from the metadata
# endpoints — see docker/entrypoint.sh. This requires the ECS EC2 launch type;
# Fargate grants neither NET_ADMIN nor anything else the sandbox needs.
#
# Shared apps are placed by the shared-mode agent (`toft agent`, internal/agent),
# which runs each under `toft runner` as its own account. What holds them is the
# agent's tier, detected at its start and said on Admin -> Info: a cgroup of
# their own where the container may create one; and only with CAP_SYS_ADMIN and
# CAP_NET_ADMIN, a network, mount and PID namespace each. Below the namespace
# tier apps share loopback and can reach each other's ports and the platform API
# as anonymous.

# Stage 1: Build frontend
#
# Pinned to BUILDPLATFORM: the output is JavaScript, which is the same bytes on
# every architecture, so running Vite under QEMU would buy nothing and cost an
# afternoon. Everything below that must NOT be pinned this way is called out
# where it appears — see the gotoolchain stage.
FROM --platform=$BUILDPLATFORM node:26-trixie-slim AS frontend
WORKDIR /build/web
COPY web/package.json web/package-lock.json* ./
# The npm cache is a mount rather than a layer: it never lands in an image, and
# a lockfile that moved by one dependency re-downloads one dependency.
RUN --mount=type=cache,target=/root/.npm \
    npm ci
COPY web/ ./
RUN npm run build

# Stage 2: Build Go binary
#
# Also pinned to BUILDPLATFORM, and cross-compiled to TARGETARCH instead. This
# is only sound because the build is CGO_ENABLED=0; the moment anything here
# needs cgo, this stage has to go back to running on the target platform.
#
# -trixie for currency rather than for correctness: this stage's base never
# reaches an image and its output is static, so it is the one golang: line here
# that was free to stay on bookworm. It moved anyway, because bookworm left
# Debian's regular security support on 2026-07-11 and a build stage is still a
# filesystem somebody's CI pulls.
FROM --platform=$BUILDPLATFORM golang:1.27-trixie AS backend
WORKDIR /build
# Pure Go since the move from go-sqlite3 to pgx — no CGO, so the binary is
# static and the runtime stage needs no libc matching.
ENV CGO_ENABLED=0
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
# Deliberately not `COPY . .`. The RUN below compiles the whole program, and
# with the tree copied in, a documentation edit, a chart change or a test
# fixture invalidated that layer — so most commits paid for a full compile that
# nothing in them could have changed. These are the paths package main is made
# of and the three trees it and its embeds read; docker/runner/Dockerfile has
# copied this narrowly since it was written.
COPY main.go cli_*.go ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY sdk/ ./sdk/
# internal/spa embeds dist/*, so the built SPA must be in place before the Go
# build. Replace rather than merge: copying onto an existing dist/ is what
# produces the stray nested dist/dist/ seen in local checkouts. .dockerignore
# excludes internal/spa/dist for the same reason, and this is the second line
# of that defence rather than a restatement of it — the COPY above would carry
# the directory in the moment somebody unignores it.
RUN rm -rf internal/spa/dist
COPY --from=frontend /build/web/dist ./internal/spa/dist
# TARGETARCH is supplied by BuildKit and is already Go's spelling (amd64,
# arm64), so it feeds GOARCH directly. GOOS is stated rather than inherited:
# this stage's own platform is pinned to the builder's, and an unstated GOOS
# would silently follow it if a build ever ran from a non-Linux host.
ARG TARGETARCH
# VERSION overrides internal/version.Version for a build that is not a release
# — a pre-release, a vendor rebuild. Empty means the compiled-in constant
# stands, which is the normal case. scripts/publish-image.sh passes this and
# the registry tag from the same string, because a tag that can disagree with
# the binary it names is the one thing the tag is for.
ARG VERSION=""
# The compile cache is keyed by TARGETARCH and the module cache is not. The two
# target builds produce different object code and would evict each other on
# every alternating build; the modules they read are the same bytes. Both are
# local to a builder and neither is carried by a registry cache export, so this
# is worth minutes on any persistent builder and nothing at all on a cold one.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build,id=gobuild-$TARGETARCH \
    GOOS=linux GOARCH=$TARGETARCH go build -trimpath \
    -ldflags="-s -w ${VERSION:+-X toft/internal/version.Version=$VERSION}" \
    -o /toft .

# Stage 2b: the Go toolchain that ships *inside* the image.
#
# Deliberately NOT pinned to BUILDPLATFORM — it follows TARGETPLATFORM, and
# that is the whole point of the stage. Shared-mode Go apps are compiled at
# deploy time inside the running container, so the toolchain the image carries
# has to run on the machine the image runs on, not on the machine that built
# it. Copying it from the `backend` stage instead — which is what this
# Dockerfile did while it was single-arch, and which reads like the obvious
# simplification — puts an amd64 toolchain in the arm64 image. Nothing fails at
# build time: the image starts, serves, passes its healthcheck and deploys Node
# and Python apps. The first Go app deployed on an arm64 install dies at
# `go build` with an exec format error.
#
# There is no RUN here, so nothing is emulated; BuildKit pulls the target
# architecture's image and COPY reads out of it.
#
# -trixie, matching the runtime stage's Debian. This toolchain is copied
# wholesale into the runtime stage and its binaries have to find *that*
# filesystem's libc, so the suffix here is load-bearing in a way the `backend`
# stage's — cross-compiled CGO_ENABLED=0, base never reaching an image — is not.
#
# What this stage is *not* is one end of the pair that decides whether a Go app's
# binary runs. Nothing but /usr/local/go leaves here; `go build` runs in the
# runtime stage below. See docker/runner/Dockerfile's family-go, and
# TestAGoBinaryIsBuiltAndRunOnTheSameDebian, which compares the right two.
FROM golang:1.27-trixie AS gotoolchain

# Stage 3: Runtime — Node.js for deployed Node and Next.js apps
#
# Node 26, the active LTS since 2026-10-28 and the default an app with no pin
# gets; 24 and 22 are carried below for apps that pin them. Before 24 it was 20:
# Node 20 left maintenance on 2026-04-30 and the
# image stopped being rebuilt, which froze both its Debian patch level and the
# npm it bundles — scripts/scan-image.sh found two CRITICALs in libgnutls30 and a
# CRITICAL in npm's vendored node-tar that no rebuild could clear, because
# upstream had stopped rebuilding.
#
# Two rules pick the replacement, and 24 is where they meet. First: the *active*
# LTS, not merely a supported one. Node 22 is still receiving fixes but entered
# maintenance in October 2025 and ends April 2027, so choosing it would buy
# eight months and then reprint this paragraph; 24 runs to April 2028. Node 26
# is newer still and is the wrong answer for the opposite reason — it does not
# become LTS until October 2026, and a hosting platform should not be the thing
# that puts someone else's app on a Current release. Second: the same major the
# isolated runner uses (docker/runner/Dockerfile's family-node), which moved to
# 24 in this same change. Shared mode and isolated mode must not hand an app two
# different Node runtimes, or an app that works in one fails in the other.
#
# -trixie rather than the bookworm default, and **the reason for that suffix has
# changed hands twice** — which is worth knowing before anyone economises on it.
#
# It was originally about *Python*: the builder ran `uv venv venv` with no
# `--python`, so the base image's Debian release silently decided what Python a
# deployed app got. On bookworm that was 3.11 while family-python was already on
# 3.12 — the same app, two Pythons, chosen by which execution mode it landed in.
# That reason is gone twice over: the builder passes `--python` explicitly on
# every build (ADR §64), and this image's Python no longer comes from its Debian
# at all (see the uv python install below).
#
# What holds the suffix now is **Go**. `go build` runs in this stage, in both
# execution modes, and the pod execs the binary — so this filesystem's glibc and
# family-go's have to match or a cgo binary dies at exec with a missing symbol
# and nothing about Debian in the message. This end is not the one free to move,
# because the Node major has to match family-node; family-go follows this.
# TestAGoBinaryIsBuiltAndRunOnTheSameDebian is the guard, and
# docs/dev-reference.md § Track the active LTS is the long version.
FROM node:26-trixie-slim AS runtime
# `upgrade` before `install`: the base image is rebuilt on upstream's cadence,
# not on ours, so between rebuilds it carries whatever security updates Debian
# has published since. Installing named packages picks up the current version of
# those; everything already in the base stays where it was. The upgrade is what
# makes the OS patch level a property of *this* build's date rather than of the
# base tag's last push — the difference that left libgnutls30 unpatched above.
# python3 is deliberately NOT in this list, and it used to be. See the uv python
# install below: this image's interpreter is a stated version at a stated path,
# not whatever the base's Debian happens to carry.
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
    apt-get update && apt-get upgrade -y && \
    apt-get install -y --no-install-recommends \
    git ca-certificates procps curl iptables iproute2
# iproute2 is the namespace tier's: the shared-mode agent plugs each workload's
# veth into its bridge with `ip` (internal/agent/netns_linux.go).
#
# uv, which is what resolves and installs a deployed Python app's dependencies.
#
# Copied from a pinned image rather than fetched with `curl … | sh`, for two
# reasons that happen to point the same way. The build no longer executes a
# shell script downloaded at build time, which is the one step here that was
# unpinned remote code with the Docker socket behind it. And the version becomes
# a *stated* fact instead of "whatever was current the day this layer was
# rebuilt" — which matters because a resolver version is not neutral: it decides
# which dependency versions an app actually gets.
#
# This pin and docker/runner/Dockerfile's must be the same string, and
# TestUvIsPinnedToTheSameVersionInBothImages fails the build when they are not.
# They were 0.12.5 and 0.5.11 for twenty months, which is how the same app came
# to be resolved by two different resolvers depending on execution mode.
COPY --from=ghcr.io/astral-sh/uv:0.13.6@sha256:af22a52f5eafef3d78e551599d405c02df61919d18028f0cea8eda93bc9986d0 /uv /usr/local/bin/uv

# Python, installed by uv rather than by apt — the one place this image departs
# from "take what the base's Debian gives you", and it departs for a reason apt
# cannot be argued out of.
#
# **3.15 does not exist in a Debian stable.** trixie's python3 is 3.13.5 and
# neither 3.14 nor 3.15 enters one before forky, while the isolated runner's
# family-python is `python:3.15-slim-trixie` today, because upstream's Python
# images are not bound to Debian's packaging. Holding this image at Debian's 3.13 would mean an app
# getting a different Python depending on which execution mode it landed in — the
# exact defect the trixie move closed once already.
#
# **Why this replaces apt's python3 rather than sitting beside it.** The toolchain
# probe takes whatever `python3` resolves to on PATH as the install *default*
# (internal/toolchain), which is what an app with no `[runtime]` pin gets. Adding
# 3.15 as a declared extra while Debian's 3.13 kept `python3` would leave every
# unpinned shared app on 3.13 and every unpinned isolated app on 3.15 — a
# divergence in the one direction no developer can see. One interpreter provider,
# one version, and the two images agree.
#
# **The path is versioned on purpose.** uv installs to
# `/opt/python/cpython-3.15.0-linux-<arch>-gnu` and maintains a *minor*-level
# symlink beside it, which is what a venv ends up bound to — so a patch bump on a
# rebuild does not dangle anything already built, and only a minor move does. The
# arch is in that name, so this adds one of its own that is stable across both
# published architectures, and the version in it is what
# TestThePlatformImageDeclaresTheSamePythonAsTheRunner reads: a declaration in a
# file that a test can hold against family-python's tag, which the Python row of
# the toolchain table went without for as long as this was "whatever Debian
# carries".
#
# `python3 -V` is not decoration: it fails the build here rather than the first
# boot if the symlink chain is wrong. The platform refuses to start on a
# TOFT_PYTHON_PATH entry that will not exec (ADR §64), and a build is a
# cheaper place to find that out.
#
# **Three of them, and the two older ones are the same decision the older R
# series are** (ADR §64 D19/D21). A pin is refused and never approximated, so an
# install carrying one interpreter can only ever say no to `>=3.13,<3.14` — and
# a Python minor is a breaking change in both directions: 3.14 removed the
# distutils shim a great many published `setup.py` files still import, and a
# wheel built for cp313 does not load on 3.15 at all. An app that ran somewhere
# else last year is pinned to what it ran on, and the platform either has it or
# tells the developer to rewrite their dependencies.
#
# 3.14 and 3.13 rather than "two more": they are the two minors directly below
# the current one, so this is the supported window and not an archive. 3.12 still
# receives security fixes to October 2028 and was dropped when 3.15 arrived, to
# keep the set at three: an app pinned to it is refused by name and moves up.
# Both are uv's own standalone builds, the same provider as 3.15 above, so all
# three link the same way and none of them is Debian's.
#
# 196 MB for the pair — measured on this image, beside the R series' 123 MB and
# pandoc's 189 MB. It is the one lever that reaches an app whose dependencies
# are older than this image.
#
# **pip is upgraded in each of them**, for the reason npm is below: uv's builds
# carry the pip they were cut with, and pip vendors its own certifi, urllib3 and
# setuptools. scripts/scan-image.sh found a CRITICAL there (certifi 2026.06.17,
# GHSA-6q48-6c7w-9gw2) that only a newer pip clears. A floor rather than a pin:
# the floor is the first release that cleared it, and anything newer is welcome.
# --break-system-packages because uv marks these interpreters EXTERNALLY-MANAGED,
# which exists to stop an app installing into them — upgrading the installer the
# image ships is the one install that marker is not about.
RUN uv python install --install-dir /opt/python 3.15 3.14 3.13 && \
    for v in 3.15 3.14 3.13; do \
      mkdir -p /opt/python/$v/bin && \
      ln -s /opt/python/cpython-$v-*/bin/python3 /opt/python/$v/bin/python3 && \
      /opt/python/$v/bin/python3 -V && \
      /opt/python/$v/bin/python3 -m pip install --no-cache-dir --break-system-packages \
        --root-user-action=ignore --upgrade 'pip>=26.2.2' && \
      /opt/python/$v/bin/python3 -m pip --version; \
    done && \
    ln -s /opt/python/3.15/bin/python3 /usr/local/bin/python3 && \
    python3 -V
# What the operator would otherwise have to state themselves, stated by the image
# that knows the answer. An enterprise building on top of this replaces it to
# offer more than one interpreter — docs/operations.md § Extra language versions.
#
# **Highest first, and the first entry is the default** — the one an app with no
# pin gets. On this image that is also what `python3` resolves to on PATH, by
# the symlink above, so the order here is documentation rather than mechanism;
# it is the order a reader will copy into their own image, where it decides.
ENV TOFT_PYTHON_PATH=/opt/python/3.15/bin/python3:/opt/python/3.14/bin/python3:/opt/python/3.13/bin/python3

# R, which is what restores a deployed R app's renv library. From apt rather
# than from a version-pinned upstream, which is the opposite of the Python
# decision above and is right for the opposite reason.
#
# Python departed from Debian because 3.15 does not exist in a Debian stable,
# and since R 4.6 R has departed too, for the same reason: trixie's apt stops at
# 4.5. Every R in both images is now Posit's build for debian-13 — the provider
# the older series already came from — at /opt/R/<version>, with the newest
# linked onto PATH as the default. One provider for all three, the same patch
# strings in both Dockerfiles, and TestBothImagesCarryTheSameRSeries holding the
# sets equal; what apt used to settle by construction is now a test.
#
# The coupling that does matter is the library rather than the interpreter. A
# binary R package is compiled against a distribution's system libraries, and
# the renv library that ships in a release is restored *here*, on the platform
# host, in both execution modes — so this stage and family-r have to be the same
# Debian or the release unpacks and then fails at dyn.load inside the pod.
# TestTheDeclaredRunnerRMatchesTheImage holds the declaration against the stage.
#
# No compiler *on this line*, and the compiler arrives on its own RUN
# below — see "The compiler, and why this image now carries one". The line here
# is the system-library list and is held equal to family-r's; the toolchain is
# the platform's alone, because a restore happens here in both execution modes
# and never in a pod.
#
# libuv1t64 and libsodium23 are the price of that settlement. A binary R package
# links the distribution's shared libraries rather than vendoring them, so the
# packages P3M hands over are only installable here if what they link is
# present: trixie's `fs` binary needs libuv.so.1 and `sodium` needs
# libsodium.so.23, and between them they are in the dependency closure of all
# three R frameworks — shiny, plumber and rmarkdown. Without them a restore gets
# as far as renv's install test and fails at dyn.load, one package at a time,
# which is the failure the compiler decision below is designed to avoid and
# reads identically to it.
#
# t64 on libuv1 is trixie's 64-bit time_t rename, not a variant choice.
#
# **The eight names after them are the second answer to the same question, and
# they reverse the first.** This list said two, and said why: an unbounded apt
# list in an image is a supply chain nobody reviews, and *an app reaching
# further is a build failure naming the library, which is the right place to
# learn it*. The first half of that still holds and is why this is still a list
# rather than a `sysreqs` sweep. The second half did not survive contact. Two
# real Shiny apps reached further on their first deploy, and what they got was
# `libproj.so.25: cannot open shared object file` fifteen frames into an renv
# traceback, naming a package the author never wrote down — and nothing they
# could do about it, because nobody deploying an app can apt-get into an image.
# A build failure is the right place to learn something the reader can act on.
#
# So the boundary moves from *two* to *named, with the R package that needs it*,
# which is reviewable and is the only property the original argument needed:
#
#   libgdal36 libproj25 libgeos-c1t64   sf, terra   — anything drawing a map
#   libudunits2-0                       units       — and sf depends on units
#   libgit2-1.9                         gert        — credentials, usethis
#   libabsl20240722                     s2          — sf's spherical geometry
#   libglpk40                           igraph      — graphs, tidygraph
#   libxml2                             xml2        — and igraph's reader
#   libwebpmux3 libfreetype6            ragg        — ggplot2's raster device
#   libpoppler-cpp2                     pdftools    — reading a PDF
#   libnetcdf22                         ncdf4, RNetCDF — gridded science data
#   libnode115                          V8          — JavaScript inside R
#
# **freetype is named here for the header, not the library**, which is the one
# entry on this list that is not what it looks like. libfreetype.so.6 arrives
# transitively whatever this line says, so a *binary* ragg has always loaded —
# and that is why its absence went unseen. A *source* ragg configures against
# freetype2, libpng, libtiff-4 and libjpeg; the last three had their -dev pulled
# in by something else and freetype did not, so the build died at
# `fatal error: ft2build.h: No such file or directory` on an image whose R could
# otherwise compile anything. Naming the runtime library is what earns the -dev
# peer below, under the rule that list is held to.
#
# Latent until multi-R made it reachable: nothing compiles ragg while every
# lockfile resolves to the R its binaries were built for, so this and the R
# versions below are one commit for one reason.
#
# The last three are the same lesson a third time, and they were paid for the
# same way: thirty real Shiny applications were deployed at once and twelve of
# them died at `libwebpmux.so.3: cannot open shared object file`. ragg is not
# an exotic dependency — it is ggplot2's raster device, which means it is in
# the closure of an enormous fraction of everything R draws.
#
# liblapack3 and libblas3 are deliberately *not* named: Posit's R deb pulls
# OpenBLAS, which provides both, and naming them here would be a second
# declaration free to disagree with the interpreter's about which BLAS it links.
#
# The list is an ARG rather than words on the apt line so that the two images'
# copies can be compared as one string: TestBothImagesInstallTheSameRSystemLibraries
# reads R_SYSTEM_LIBRARIES from both files.
#
# 136 MB, said out loud beside pandoc's 189 MB below. The two are the same trade
# — pandoc is the only way one framework renders at all, and this is the only
# way an ordinary and very large class of Shiny app installs at all.
ARG R_SYSTEM_LIBRARIES="libuv1t64 libsodium23 \
    libgdal36 libproj25 libgeos-c1t64 libudunits2-0 libgit2-1.9 libabsl20240722 \
    libglpk40 libxml2 libwebpmux3 libfreetype6 libpoppler-cpp2 libnode115 libnetcdf22"
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    rm -f /etc/apt/apt.conf.d/docker-clean && \
    apt-get update && \
    apt-get install -y --no-install-recommends $R_SYSTEM_LIBRARIES

# --- The compiler, and why this image now carries one ----------------------------
#
# **This reverses a decision, and the argument it reverses was a good one.** It
# said: R packages come from P3M as binaries, a binary needs no compiler, and an
# image carrying gcc, g++, gfortran and make is several hundred megabytes of
# supply chain nobody reviews. Every clause of that is still true. What was
# wrong was the premise underneath it — that a binary is always available.
#
# config.DefaultRRepos names a snapshot, and a snapshot serves a binary for the
# version that is current *in it* — dated or not. renv restores the version the
# lockfile pins. Those are the same version for a repository that resolved its
# lockfile recently, and they are not the same version for anything else — and
# "anything else" is most of the real world: a Posit Connect manifest.json
# records the exact versions the author had on the day they published, and that
# day is usually years ago. Every one of those pins is a source build.
#
# Thirty published Shiny applications were deployed to a real install to find
# this out. Ten died at `sh: 1: make: not found`, after installing between
# eleven and fifty-nine packages successfully from binaries first — so the
# failure arrived several minutes in, naming a shell builtin, at the bottom of
# an renv traceback, on packages like `digest` and `Rcpp` that no author thinks
# of as needing a compiler. Nothing the person deploying could do would have
# fixed it, which is the same test the eight system libraries above had to meet.
#
# build-essential and gfortran, which is Debian's own declaration of a C, C++
# and Fortran toolchain. **Not r-base-dev**, which was this line until R 4.6:
# it depends on r-base-core, so it would install trixie's R 4.5 at
# /usr/bin/Rscript as a fourth interpreter beside the three this image declares.
# Posit's R debs below depend on gcc, g++, gfortran and make themselves, and
# carry R's headers; naming the toolchain here anyway is what lets the check at
# the end of this RUN pass before they are installed.
#
# The -dev headers after it are **the -dev peers of the runtime libraries this
# image already names, and nothing else**. That is a rule rather than a
# wishlist, which is what keeps this reviewable: the runtime list is chosen and
# argued above, and a source build of the package that needed the library needs
# that library's headers. Adding a name here means adding it there first.
#
# libssl-dev and libcurl4-openssl-dev are the two exceptions, and they are the
# two R packages compile against more than anything else — `openssl` and `curl`
# are in the dependency closure of every framework this platform hosts. Their
# runtimes are already here (Posit's R deb pulls libcurl, and libssl3 is in
# the base).
#
# **Only the platform image.** The restore happens on this host in both
# execution modes and its result is copied into the release, so family-r needs
# what a compiled package *links* — which is the list above, held equal by
# TestBothImagesInstallTheSameRSystemLibraries — and never what compiled it.
# Putting a toolchain in the runner would be several hundred megabytes in every
# pod for nothing.
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    rm -f /etc/apt/apt.conf.d/docker-clean && \
    apt-get update && \
    apt-get install -y --no-install-recommends build-essential gfortran \
    libssl-dev libcurl4-openssl-dev \
    libuv1-dev libsodium-dev libgdal-dev libproj-dev libgeos-dev libudunits2-dev \
    libgit2-dev libabsl-dev libglpk-dev libxml2-dev libwebp-dev libfreetype-dev \
    libpoppler-cpp-dev libnetcdf-dev libnode-dev && \
    command -v make >/dev/null && command -v gcc >/dev/null && command -v gfortran >/dev/null || \
    { echo "the platform image cannot build an R package from source"; exit 1; }

# The older R series, and the reason this image carries more than one.
#
# **A binary R repository publishes a package version only for the series it was
# current in.** Ask P3M for `bit 4.0.5` as R 4.4 and it redirects to a binary;
# ask for the same file as R 4.5 and it hands back source, which then compiles
# against 4.5's headers — where `Calloc`, `Free`, `Realloc` and `PI` no longer
# exist. So a Connect bundle pinned to 2023 package versions does not merely
# build slowly on a newer R; a predictable slice of it does not build at all.
# Measured on eight real EPA bundles deployed to this platform: three failed on
# exactly that, one more on ragg's missing header above, and every one of them
# had stated the R it wanted in its own manifest.
#
# **Posit's builds, for debian-13, which is this image's Debian.** That is the
# whole reason this is affordable: r-builds publishes the same trixie the FROM
# line names, so these interpreters link the same system libraries as the apt R
# beside them and family-r loads what they restore. A build for another
# distribution would reintroduce, on purpose, the exact mismatch
# TestBothImagesTakeRFromTheSameDebian exists to prevent — which is what the
# rhel9 compat shims on the AL2023 dev box cost, and they are not shipped.
#
# **All three are Posit's, and the newest is the default.** They install to
# /opt/R/<version>, and the newest is linked into /usr/local/bin as R and
# Rscript — which is what makes it the default, because toolchain's probe takes
# the Rscript on PATH as the one an app with no declaration gets. Until R 4.6
# the default was apt's and these were additions beside it; trixie's apt stops
# at 4.5, so the default moved to the provider the older series already came
# from. Their headers come with them, so the toolchain above compiles for all
# three.
#
# About 65 MB of .deb each, said out loud beside the 136 MB of system
# libraries and pandoc's 189 MB. It buys the same thing the system libraries
# bought — an ordinary and very large class of real R application installing at
# all — and it is the only lever that reaches the isolated half, where
# TOFT_R_PATH on this host decides nothing (ADR §64 D19).
#
# **Each .deb is checked against a SHA-256 written here** before apt sees it —
# version:amd64:arm64 — because the CDN serving it is not the thing being
# trusted; this file is. Posit publishes no checksum beside these, so the
# hashes are of the files as first fetched (2026-10-28), and a .deb rebuilt
# under the same name fails the build rather than shipping. Bumping a version
# means fetching its two .debs and writing their `sha256sum` here.
ARG TARGETARCH
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    set -eux; \
    rm -f /etc/apt/apt.conf.d/docker-clean; \
    for entry in \
      4.6.2:e8fe0cace604d1b44515522cde5440a0937301f2660f282f908cb1e0deef15e8:1c4992e23c48a6846777b12b221e6333efb6c011e68ef88b797dc5512969e113 \
      4.5.3:5fd9f26826a14b4a73c17aa6656ad3ee52578acddcb896a76816af8ef604c5b6:864b0d0a1045d181fa7790c2a2352196bd37d1be993f7eea9cd1efe7086d07e5 \
      4.4.3:d8b8ea1d3169ad840d15c3cd276cd9fbc250cad1ac36e1a58f8798e63e592169:82d2b2bdd6e5f3443b06176f82a9ea8386759f0afb96a23e077dba46232c18e9; do \
      v=${entry%%:*}; sums=${entry#*:}; \
      case "$TARGETARCH" in amd64) sum=${sums%%:*} ;; arm64) sum=${sums#*:} ;; *) echo "no R build for $TARGETARCH" >&2; exit 1 ;; esac; \
      url="https://cdn.posit.co/r/debian-13/pkgs/r-${v}_1_${TARGETARCH}.deb"; \
      curl -fsSL -o /tmp/r.deb "$url"; \
      echo "$sum  /tmp/r.deb" | sha256sum -c -; \
      apt-get update && apt-get install -y --no-install-recommends /tmp/r.deb; \
      rm -f /tmp/r.deb; \
      "/opt/R/${v}/bin/Rscript" -e 'cat(R.version.string)' >/dev/null; \
    done; \
    ln -s /opt/R/4.6.2/bin/R /usr/local/bin/R; \
    ln -s /opt/R/4.6.2/bin/Rscript /usr/local/bin/Rscript; \
    got=$(Rscript -e 'cat(R.version$major, ".", sub("[.].*", "", R.version$minor), sep="")'); \
    [ "$got" = "4.6" ] || { echo "the platform image's default R is $got, not 4.6"; exit 1; }

# openssl, for the same reason family-r installs it and a different deployment:
# in **shared mode** an R app runs as a process on this host rather than in a
# pod, so the SDK's caller_role verifies its HMAC with *this* image's R. The
# one-box install is shared mode, so leaving this to the runner would ship a
# verifier that works in k3s and returns "unknown" for every request on the
# install most customers start with.
#
# Into **every** R's .Library — an app runs under the R its lockfile names, not
# the default, and an R 4.5 app's SDK needs openssl as much as a 4.6 one's — from the P3M snapshot config.DefaultRRepos
# names, under the User-Agent internal/runner/rlaunch.go sends — P3M answers an
# unmodified R with source. .Library rather than a site library because both
# launchers set R_LIBS_SITE to the *release's* library, and a package anywhere
# else is invisible once they do; the check runs under that condition.
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
      R_LIBS_SITE=/nonexistent "$r" -e 'if (!requireNamespace("openssl", quietly = TRUE) || length(openssl::sha256(charToRaw("x"), key = charToRaw("k"))) != 32L) { cat("the platform image cannot compute an HMAC where a deployed app runs, under", R.version.string, "\n"); quit(status = 1) }'; \
      rm -rf "${r%/bin/Rscript}"/lib/R/library/*/doc; \
    done

# Stated by the image that knows the answer, as TOFT_PYTHON_PATH is above.
# Rscript rather than R: it is the non-interactive front end and it is what every
# launcher and build step execs.
#
# **Highest first, the default first of all**, because toolchain's probe takes
# the binary on PATH as the default and dedupes the rest on the series. The
# order here is therefore documentation rather than mechanism on this image —
# but it is the order a reader will copy into their own, where it does decide.
ENV TOFT_R_PATH=/opt/R/4.6.2/bin/Rscript:/opt/R/4.5.3/bin/Rscript:/opt/R/4.4.3/bin/Rscript

# pandoc, because `rmarkdown::render` is a pandoc front end and refuses without
# one: an R Markdown deploy that installs its 24 packages in six seconds then
# fails with "pandoc version 1.12.3 or higher is required", which names a tool
# nothing in the repository asked for.
#
# **Here for the build, and in family-r for one workload.** An R document is
# rendered once at build time, on the platform host, and what ships to the pod
# is the directory that fell out — served by `staticserve`, which needs no R and
# no renderer. The exception is a Shiny document (`runtime: shiny`), which
# `rmarkdown::run` renders at its first request wherever it runs, so family-r
# carries pandoc too (ADR §64).
#
# 189 MB is the cost and it is worth saying out loud next to §65's argument for
# keeping Quarto's 446 MB out of the default tag. The two are not the same
# trade: Quarto is a second engine an install may decline and a `[notebook]`
# pin is refused by name when it is absent, whereas pandoc is the only way the
# rmarkdown framework renders at all — a default image without it detects R
# Markdown, builds it, and fails it. The quarto stage carries its own pandoc
# under /opt/quarto and does not use this one; that duplication is the price of
# the stage being `FROM runtime` rather than a rebuild.
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    rm -f /etc/apt/apt.conf.d/docker-clean && \
    apt-get update && \
    apt-get install -y --no-install-recommends pandoc && \
    pandoc --version | head -1

# npm is upgraded away from the one bundled with the Node image for the same
# reason, and it is not cosmetic: npm is what runs against a *deployed app's*
# package.json, so its own vendored tar/glob/minimatch are on the path that
# handles repository content. Separate invocations — npm replacing itself in the
# same command that installs another package is a documented way to end up with
# a half-written global tree.
#
# A floor inside the major rather than `@latest`, because `latest` lags: on
# 2026-10-21 it was 12.2.0 while 12.2.1, which carries fixed ip-address,
# brace-expansion, postcss-selector-parser and undici, sat on `next-12`. A
# range takes the highest 12.x the registry has, so it still moves forward.
RUN npm install -g 'npm@^12.2.1' && \
    npm cache clean --force

# The older Node lines, and the reason this image carries more than one.
#
# **A native module is compiled against one major's ABI.** `npm rebuild` links
# against the headers of the Node running it, and the result is loaded by
# `require` — so a package with a binding built for 20 and executed by 24 dies
# at `dyn.load`'s Node equivalent, naming a `.node` file. Pure JavaScript is
# usually portable and a great deal of published JavaScript is not pure: an app
# with `better-sqlite3`, `canvas`, `bcrypt` or `sharp` in its tree is pinned to
# the major it was written for, and until now this platform's answer to
# `[runtime] node = ">=20,<21"` was to refuse the deploy.
#
# **24 and 22, which are the two LTS lines below the one above.** 24 is in
# maintenance and supported to April 2028, 22 to April 2027. 20 was carried past
# its end of life (2026-04-30) until 26 became LTS on 2026-10-28, and was
# withdrawn then to keep the set at three, as Python 3.12 was when 3.15 arrived:
# an app that pins it is refused by name and moves up. Drop a major from the
# loop and from TOFT_NODE_PATH to withdraw it.
#
# From nodejs.org's own tarballs rather than from a package manager, for the two
# reasons the Python above is uv's: neither major exists in a Debian stable, and
# a tarball is a stated version at a stated path rather than whatever a
# repository moved to overnight. The `.tar.gz` rather than the smaller `.tar.xz`
# beside it, because this base carries no `xz` — Quarto's download below made
# the same choice, and paying 20 MB of transfer once at build time is cheaper
# than an apt install and a purge around it. They land in /opt/node/<major> and touch
# nothing in /usr, so the image's own Node stays the interpreter on PATH and the
# default an app with no pin gets — additions rather than a replacement, exactly
# as the R series are.
#
# **npm is upgraded in each of them**, for the reason it is upgraded above and
# not as housekeeping: npm is what runs against a *deployed app's*
# package.json, so its own vendored tar and undici are on the path that handles
# repository content, and the one a two-year-old tarball ships is two years
# old.
#
# **The ceiling is stated per major, and that is not fussiness.** npm 12 (`^12.2.1`,
# the floor the runtime stage's own npm takes, for the reason given there) is whose own engines are `^22.22.2 || ^24.15.0 || >=26.0.0` — so it
# installs on 24 and 22 alike (20, which it refused with EBADENGINE, is gone), which is the correct
# behaviour of a tool that knows what it runs on. npm 11 is the last major that
# supports Node 20 (`^20.17.0 || >=22.9.0`) and is frozen there. Naming the
# ceiling beside the version is what keeps this a build that either works or
# fails loudly; the alternative is `|| true`, which ships whatever npm the
# tarball was cut with and calls it a decision.
#
# 366 MB for the pair, measured. The same trade as the R series' 123 MB and the
# system libraries' 136 MB, bought for the same thing: an ordinary class of real
# application running at all.
#
# **Each tarball is checked against the SHA-256 written here**
# (version:npm:x64:arm64, from that release's SHASUMS256.txt) before it is
# unpacked — to a file first, never `curl | tar`, so a bad download cannot
# leave half a tree behind a passing check. The npm range is a moving target
# on purpose (see the runtime stage's own npm above) and is not pinned.
ARG TARGETARCH
RUN set -eux; \
    case "$TARGETARCH" in \
      amd64) arch=x64 ;; \
      arm64) arch=arm64 ;; \
      *) echo "no Node build for $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    for entry in \
      24.22.2:^12.2.1:361718c444e49af2ca5a110c83ba6a8901143f23ab285ebbef0341f391f482a9:95c264150aceb7f3437a29fd6984924f5f8d1511ea3cbd6c508b83f6c555dae2 \
      22.23.4:^12.2.1:b8492284c76a54baf58d61668dd0e41ee0fc98adabf9503d30f05d08f55b4406:f7a1e486dbc8b286847815b477d4fb5e9fb963ed4489c3e1588853d685f589f9; do \
      v=${entry%%:*}; rest=${entry#*:}; npm_tag=${rest%%:*}; sums=${rest#*:}; major=${v%%.*}; \
      case "$arch" in x64) sum=${sums%%:*} ;; arm64) sum=${sums#*:} ;; esac; \
      mkdir -p "/opt/node/$major"; \
      curl -fsSL -o /tmp/node.tar.gz "https://nodejs.org/dist/v${v}/node-v${v}-linux-${arch}.tar.gz"; \
      echo "$sum  /tmp/node.tar.gz" | sha256sum -c -; \
      tar xzf /tmp/node.tar.gz -C "/opt/node/$major" --strip-components=1 \
            --exclude='*/share/doc' --exclude='*/share/systemtap' --exclude='*/CHANGELOG.md'; \
      rm -f /tmp/node.tar.gz; \
      "/opt/node/$major/bin/node" -v; \
      PATH="/opt/node/$major/bin:$PATH" "/opt/node/$major/bin/npm" install -g "npm@${npm_tag}"; \
      "/opt/node/$major/bin/npm" cache clean --force; \
    done
# Where they are, in the form the probe reads — the Node peer of
# TOFT_PYTHON_PATH above, and absent until this image had more than one.
#
# Highest first, and this image's own Node first of all: the probe takes the
# binary on PATH as the default and dedupes the rest on the series, so the entry
# for 24 is documentation on this image and mechanism on one a reader builds.
ENV TOFT_NODE_PATH=/usr/local/bin/node:/opt/node/24/bin/node:/opt/node/22/bin/node
# Shared-mode Go apps are compiled at deploy time *inside this container*
# (builder runs `go build` → toft-app), so the toolchain must ship in the
# runtime image like node and uv do. From the gotoolchain stage, never from
# `backend` — read that stage's comment before changing this line. Same Debian
# base and the same pinned tag as the build, so the toolchain that ships is
# still the one the platform was built with.
COPY --from=gotoolchain /usr/local/go /usr/local/go
ENV PATH="/usr/local/go/bin:${PATH}"

# The older Go toolchains, and **Go's older versions are not the same decision
# as Python's and Node's** — which is the first thing to know before reading the
# rest of this, because the shape of the problem is different in every part.
#
# A `go.mod` directive is a **minimum**: `go 1.25` means *at least* 1.25, so
# `GoModPin` reads it as `>=1.25` and the resolver picks the highest that
# satisfies it. That is Go's own semantics and it is right — Go does not break
# backwards compatibility, and a module declaring 1.25 compiles under 1.27 with
# its language version pinned to 1.25 by that very directive. **So nothing here
# changes which toolchain an ordinary build picks**, and a reader expecting the
# Python and Node story below will be wrong about this one. There is no
# `[runtime] go` key and there will not be (ADR §64), and no `RUNNER_GO_VERSION`
# either, because `go build` produces a binary the workload execs.
#
# What these are for is **`GOTOOLCHAIN=<name>`, the operator's own setting**, and
# getting that wrong is easy enough that it is worth stating what was measured.
#
# **Go never downgrades.** A `toolchain go1.26.9` line in a go.mod does not
# select 1.26.9 when the local toolchain is 1.27: Go takes the *newer* of the
# two, always, so a module naming an older toolchain is simply built by the
# newer one. Measured in this image — `GOTOOLCHAIN=path` with that exact
# directive produced a `go1.27.0` binary. So no directive an app can write
# reaches these, which is the same sentence as "there is no `[runtime] go` key"
# arriving from Go's side rather than this platform's.
#
# `GOTOOLCHAIN=go1.26.9` does reach them, and it is an operator's value from
# the allow-list — build parameters are the operator's, never the app's — for
# an estate that has to be held on a known toolchain, or one dependency that
# will not compile on the newest. Go looks for an executable of that name on
# PATH before it considers the module proxy, which is what makes the shims
# below the whole of the air-gapped story: measured with `--network none`, a
# build under `GOTOOLCHAIN=go1.26.9` produces a `go1.26.9` binary and reaches
# nothing.
#
# `GOTOOLCHAIN=path` is still worth setting on an air-gapped install and the
# shims are still what it looks for — but it helps a module asking for a
# *newer* toolchain, which is the direction Go switches in, and this image
# carries none of those. The two settings are not the same lever and this line
# serves the first.
#
# Declared in TOFT_GO_PATH as well, so the platform's own probe reports
# them on the system checks page — an operator should be able to see what their
# image carries without reading this file.
#
# **The excluded `.pem` is Go's own and the exclusion is about a scanner, not a
# secret.** `src/crypto/x509/platform_root_key.pem` is a test fixture Google
# publishes in every Go source tree — its private half is on go.dev — and
# Trivy's secret scanner has a built-in allow-rule for it at Go's canonical
# `/usr/local/go`. It does not know this path, so shipping it here turned a
# published image's scan into "2 secrets" on a file that is neither. Dropping it
# is the honest fix: nothing but Go's own x509 tests reads it, and this image
# does not run those. Verified — the file is gone and `crypto/x509` still
# compiles.
#
# **One older toolchain, not two.** Go supports its two newest releases (1.27
# and 1.26) and nothing else: 1.25 stopped getting fixes, and the 1.25.14 this
# image carried brought 27 HIGH standard-library findings to every scan with no
# fix that would ever reach it. Carrying an unsupported toolchain for an operator
# to opt into is offering them the vulnerabilities. When 1.28 ships, 1.26 leaves
# the same way.
#
# About 200 MB, measured — half of what the pair cost — and still among the
# largest single lines in this image after the R libraries. Said out loud because
# it buys the narrowest thing on this page: nothing at all until an operator names
# it, and on a connected install `GOTOOLCHAIN=go1.26.9` would have fetched it
# anyway. What is bought is the air-gapped case and the second of latency, and an
# install that wants neither drops one entry from one variable.
#
# Checked against the SHA-256 written here (version:amd64:arm64, from
# go.dev/dl/?mode=json) before it is unpacked, as the Node tarballs are.
ARG TARGETARCH
RUN set -eux; \
    for entry in 1.26.9:42d158b4d8f7b61ac0a830567c940a86098fb7aac52e467a5ebec03ef5cc2f8d:4a97373d49fcacdcf3694fea368a500b00ee3e963974f3e7514132717632f052; do \
      v=${entry%%:*}; sums=${entry#*:}; \
      case "$TARGETARCH" in amd64) sum=${sums%%:*} ;; arm64) sum=${sums#*:} ;; *) echo "no Go build for $TARGETARCH" >&2; exit 1 ;; esac; \
      mkdir -p "/opt/go/$v"; \
      curl -fsSL -o /tmp/go.tar.gz "https://go.dev/dl/go${v}.linux-${TARGETARCH}.tar.gz"; \
      echo "$sum  /tmp/go.tar.gz" | sha256sum -c -; \
      tar xzf /tmp/go.tar.gz -C "/opt/go/$v" --strip-components=1 --exclude='*/test' --exclude='*/api' \
            --exclude='*/src/crypto/x509/platform_root_key.pem'; \
      rm -f /tmp/go.tar.gz; \
      "/opt/go/$v/bin/go" version; \
      printf '#!/bin/sh\nexec /opt/go/%s/bin/go "$@"\n' "$v" > "/usr/local/bin/go$v"; \
      chmod 755 "/usr/local/bin/go$v"; \
      GOTOOLCHAIN=path "go$v" version; \
    done
# Highest first, and the image's own first of all — the same order and the same
# reason as the two lists above, and here it is documentation rather than
# mechanism because a `go` directive is a minimum and the highest always wins.
ENV TOFT_GO_PATH=/usr/local/go/bin/go:/opt/go/1.26.9/bin/go

# --- Quarto ------------------------------------------------------------------
#
# **One image, and this reverses ADR §73.** Quarto used to be a second published
# tag, `<version>-quarto`, because it is 446 MB of deno, pandoc, typst and
# dart-sass that nothing but a Quarto document ever executes, and a `.qmd` on an
# install without it is refused by name rather than mis-rendered. That argument
# was about *size*, and it was the right shape for a §73-era image; what changed
# is the image it is a percentage of. The platform image carries three Pythons,
# three Nodes and three Go toolchains now, so Quarto is no longer the line item
# that decides whether this thing is large.
#
# What the split cost is the part that does not show up in a size table: two
# tags is two artifacts to build, scan, publish and keep in step, and **an
# operator has to know before installing whether anyone will ever deploy a
# `.qmd`** — a question about a document nobody has written yet, answered by
# choosing a tag. Getting it wrong is not an error message, it is a deploy
# refused weeks later on an install that has to be replaced to fix it. One image
# means the answer is always yes.
#
# **Where it lives is unchanged and is still the whole of the other half of
# §73.** Not the app's venv: Publish packs the release directory, so it would be
# in every release's artifact, re-fetched on every pod start, multiplied by
# version retention. Not the runner images: the build runs on the platform host
# in *both* execution modes and a rendered notebook's workload is a file server,
# so it would grow the Python image every Flask, FastAPI and Streamlit app also
# pulls, for a tool none of them runs. It belongs where the build runs, and this
# is that image.
#
# **The capability is still discovered, not assumed.** `QUARTO_BIN` is how the
# platform finds it and an absent Quarto still refuses a `.qmd` by name — an
# operator who builds their own image without this layer gets exactly the
# behaviour the second tag used to give them, and the refusal copy did not have
# to change. That is what makes this a packaging decision rather than a
# behavioural one.
#
# QUARTO_BIN is set here for the same reason TOFT_PYTHON_PATH is above —
# what the operator would otherwise have to state themselves, stated by the
# image that knows the answer. An operator installing Quarto on their own host
# sets it themselves; the platform never guesses at a path.
#
# The tarball rather than the .deb: two architectures, one URL shape, and no
# package manager deciding what else to bring. The version is pinned for the
# reason uv's is — a renderer version is not neutral, it decides what a document
# actually looks like — and `quarto --version` fails the build here rather than
# at the first deploy that needed it.
#
# The two SHA-256s are the release's own quarto-<version>-checksums.txt, and
# move with QUARTO_VERSION: a version bump that leaves them is a build that
# fails at the check, which is the point.
ARG QUARTO_VERSION=1.10.19
ARG QUARTO_SHA256_AMD64=8e38c2b2df6a6dbd369ea16e15acded6894545258d13a02f777f81ad18fd7973
ARG QUARTO_SHA256_ARM64=bba2bb55add74c754c313905df42b0f45a8250aee2110a4fce7f83020bfe1b8f
RUN set -eux; \
    case "$TARGETARCH" in \
      amd64) arch=amd64; sum=$QUARTO_SHA256_AMD64 ;; \
      arm64) arch=arm64; sum=$QUARTO_SHA256_ARM64 ;; \
      *) echo "no Quarto build for $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    mkdir -p /opt/quarto; \
    curl -fsSL -o /tmp/quarto.tar.gz "https://github.com/quarto-dev/quarto-cli/releases/download/v${QUARTO_VERSION}/quarto-${QUARTO_VERSION}-linux-${arch}.tar.gz"; \
    echo "$sum  /tmp/quarto.tar.gz" | sha256sum -c -; \
    tar xzf /tmp/quarto.tar.gz -C /opt/quarto --strip-components=1; \
    rm -f /tmp/quarto.tar.gz; \
    /opt/quarto/bin/quarto --version
ENV QUARTO_BIN=/opt/quarto/bin/quarto

# --- Security updates, refreshed every day ----------------------------------
# The `apt-get upgrade` near the top of this stage runs once and is then a
# cached layer, so a Debian security fix published after it — perl and gzip
# shipped +deb13u1 between two builds of 0.7.1 — never reaches an image built
# from that cache: the scan finds 29 fixable findings in an image whose
# Dockerfile says it upgrades. APT_REFRESH is the day the build runs
# (scripts/publish-image.sh and scripts/k3s.sh pass `date -u +%F`), so this one
# small layer re-runs once a day and upgrades everything installed above it,
# while the expensive layers before it stay cached. Last before the binary, so a
# code change alone never re-runs it.
ARG APT_REFRESH=unset
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    echo "security updates as of ${APT_REFRESH}" && \
    apt-get update && apt-get upgrade -y && \
    apt-get autoremove -y

WORKDIR /app
COPY --from=backend /toft /app/toft
# On PATH, because WORKDIR is not. Every recovery instruction the product prints
# — the chart's NOTES.txt, `docker compose exec` in the compose bundle — spells
# it `toft local-admin enable`, and without this that resolves to
# "executable file not found": the one command an operator reaches for when
# nobody can sign in, failing at exactly that moment. A symlink rather than
# extending PATH, so the working directory does not become a place binaries are
# found.
RUN ln -s /app/toft /usr/local/bin/toft
# --chmod: the exec bit must not depend on the checkout's file modes — a tree
# copied without permissions (rsync/scp/zip) would otherwise build an image
# whose entrypoint fails with "permission denied" at container create.
COPY --chmod=755 docker/entrypoint.sh /app/entrypoint.sh

# Release checkouts, logs, and the shared npm/uv build caches. Mount a volume
# here so deployed apps survive container replacement. The database no longer
# lives on disk — it is Postgres, reached via DATABASE_URL.
RUN mkdir -p /data/apps /data/logs /data/cache

# Per-app process isolation is on by default: each app runs as its own
# auto-created system account (primary group toft-apps) and owns only its
# release directory. The platform runs as root, which the agent needs to start
# each runner as its account.
# BLOCK_AWS_METADATA is deliberately NOT defaulted on — it needs CAP_NET_ADMIN,
# which local `docker compose` runs don't have; set it (with the capability)
# on any AWS deployment.
#
# RUNTIME, APP_USER_PER_APP and APP_USER_AUTOCREATE are absent on purpose. The
# code's defaults are now the shipped posture — the agent, and per-app accounts when
# the platform is root — so stating them here would be three restatements of
# what config.Load already derives, in the one place a customer cannot read.
ENV APPS_DIR=/data/apps \
    LOGS_DIR=/data/logs \
    CACHE_DIR=/data/cache \
    HOST=0.0.0.0 \
    PORT=8080 \
    APPS_PORT=8081

# DATABASE_URL has no default on purpose: there is no sensible one, and config
# .Load fails loudly rather than silently pointing at the wrong database.

# 8080 platform, 8081 apps gateway. The split puts deployed apps on a different
# browser origin — collapsing it re-opens the boundary, so publish both.
# Deployed apps listen on 3100-3999 but only on loopback, reached by the
# in-process proxy, and are deliberately not published.
EXPOSE 8080 8081

# /api/v1/config needs no authentication, so it is a usable liveness probe that
# does not depend on a session.
HEALTHCHECK --interval=15s --timeout=5s --start-period=30s --retries=3 \
    CMD node -e "fetch('http://127.0.0.1:8080/api/v1/config',{headers:{'user-agent':'toft-healthcheck'}}).then(r=>process.exit(r.ok?0:1)).catch(()=>process.exit(1))"

ENTRYPOINT ["/app/entrypoint.sh"]
