# syntax=docker/dockerfile:1.7

# backup-manager container image (A3.9, extended by issue #82/B4.1).
#
# Four stages:
#   1. `build`          - the provider-neutral core binary (manager logic
#                          plus the embedded rclone packages, see
#                          core/internal/transport/rclone). Unchanged by
#                          #82: everything this stage needs still comes
#                          from core/, never from apps/ or ui/.
#   2. `frontend-build` - ui/shared's static bundle (`npm run build`).
#   3. `build-web`      - the generic Web host binary (apps/generic),
#                          which imports core/service + apps/common and
#                          embeds stage 2's output via go:embed
#                          (apps/generic/webui).
#   4. (untagged)       - the distroless runtime, carrying both binaries
#                          and nothing else.
#
# There is no `rclone` executable anywhere in this image: rclone is
# consumed as a Go module (core/go.mod pins github.com/rclone/rclone),
# imported as packages, and compiled straight into /backup-manager. See
# docs/deployment.md for how that claim is verified against the built
# image, rather than just asserted here.
#
# `/backup-manager` (stage 1) implements every execution mode FR-1/FR-24
# call for: `run`, `daemon`, `check`, `status`, `sources`, `artifacts`,
# `fetch`, `retention`, `reconcile`, `validate` and `version`.
# `/backup-manager-web` (stage 3) adds exactly one more: `serve`, the
# generic Web host (docs/EPIC-B-multi-nas.md §9.2) - the shared UI, the
# versioned API, local authentication and the backup scheduler in one
# process. Two binaries, not one with two names, because apps/generic
# needs to import apps/common/webhost and apps/common/auth/local, and
# core/'s own module cannot depend on apps/ in either direction (§7.1) -
# see apps/generic/cmd/backup-manager-web/main.go's own doc for the full
# reasoning.

# ---- core builder ---------------------------------------------------------
# Pinned by digest, not just tag, so this stage resolves to the exact same
# toolchain bytes on every rebuild. golang:1.27-bookworm matches this
# module's `go 1.27.0` directive (go.mod); a mismatched builder would either
# fail outright or, worse, silently reach out via GOTOOLCHAIN to download a
# different one mid-build, which is exactly the non-reproducible behavior
# GOTOOLCHAIN=local below forecloses.
FROM --platform=$BUILDPLATFORM golang@sha256:ded31c68586d2e49e760acc2e65a884b23d032e9bbbed0ae0c55abd3fcaf4452 AS build

# CGO_ENABLED=0: the state package's SQLite driver (modernc.org/sqlite) is
# pure Go, and rclone's local/sftp backends need no cgo either, so this
# holds for both target architectures without a C cross-toolchain.
#
# GOTOOLCHAIN=local: build with exactly the Go version baked into this
# image, never fetch a different one over the network mid-build.
#
# GOFLAGS=-mod=readonly: go.sum is the source of truth; a build must never
# silently rewrite it.
ENV CGO_ENABLED=0 \
    GOTOOLCHAIN=local \
    GOFLAGS=-mod=readonly

WORKDIR /src

# Dependency layer first, so editing application code doesn't invalidate
# the module download cache. core/ is its own Go module (see the top-of-
# file note); everything below is relative to it landing at /src's root.
COPY core/go.mod core/go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Only the packages the binary actually needs; this also keeps an
# accidental `git status`-dirty file (tests/.run scratch, etc.) that isn't
# excluded by .dockerignore from ever reaching the build context for these
# paths.
COPY core/cmd/ ./cmd/
COPY core/internal/ ./internal/
COPY core/service/ ./service/
COPY core/migrations/ ./migrations/

# Cross-compiled for the target platform requested by `docker buildx build
# --platform`, without emulating the builder itself: TARGETOS/TARGETARCH
# are supplied by buildx, and the FROM above already pinned the builder to
# run natively as $BUILDPLATFORM, so this is a native `go build` doing
# cross-compilation, not qemu emulating a whole second Go toolchain.
ARG TARGETOS
ARG TARGETARCH

# VERSION/COMMIT are build inputs, not build-time discovery: the Dockerfile
# never shells out to `git` itself (the build context may not even contain
# .git; see .dockerignore), so the same two ARGs always produce the same
# stamped binary. See docs/deployment.md for how to compute them from a
# real checkout.
ARG VERSION=dev
ARG COMMIT=none

# -trimpath: strip the builder's absolute source paths from the binary, so
# the output doesn't encode where it happened to be built.
# -buildvcs=false: don't let `go build` stamp VCS state (revision, dirty
# bit) it can read off a .git directory that may or may not be in the build
# context; VERSION/COMMIT above are the one deliberate source of that
# information instead.
# -ldflags -s -w: drop the symbol table and DWARF info; there is no
# debugger-driven use case that needs them in the shipped image, and it
# shrinks the minimal runtime image.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
        -trimpath \
        -buildvcs=false \
        -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
        -o /out/backup-manager \
        ./cmd/backup-manager

# ---- frontend builder -------------------------------------------------
# ui/shared's static bundle (docs/EPIC-B-multi-nas.md §9.2/§11): built
# once here, embedded into /backup-manager-web below via go:embed
# (apps/generic/webui). Node/npm are build-time dependencies only -
# neither ships in the runtime stage.
#
# Pinned by digest for the same reproducibility reason as the Go builder
# above. Not $BUILDPLATFORM-native the way the Go stage is: a Vite build
# produces the same platform-independent static output (HTML/CSS/JS)
# regardless of which architecture ran `npm run build`, so there is no
# cross-compilation concern here to justify pinning a build platform.
FROM node@sha256:8a34c4ab3ea2c5cd194f07e317b2a8f09461d3c8b05c4e34c8ccd56d56024c4d AS frontend-build

# WORKDIR /src (not /src/ui/shared): ui/shared/vite.config.ts resolves its
# "@platform-entry" alias, and some of ui/shared's own test files
# reference a provider bridge directly, as repo-root-relative paths
# (../../apps/<platform>/frontend/...) - the same relative structure
# apps/generic's own go.mod replace directives need in the Go builder
# stage above, for the same underlying reason: something under this repo
# resolves a sibling directory by relative path, so the build context has
# to preserve that shape rather than flattening it.
WORKDIR /src

# Dependency layer first, matching the Go stages' own caching shape.
COPY ui/shared/package.json ui/shared/package-lock.json ./ui/shared/
RUN --mount=type=cache,target=/root/.npm \
    cd ui/shared && npm ci --no-audit --no-fund

COPY ui/shared/ ./ui/shared/
COPY apps/ ./apps/

# The per-provider bundles the runtime stage carries (issue #180, packaged
# by #169). One directory per platform whose distribution adapter has no
# file payload of its own to ship a bundle in: a TrueNAS catalog entry, an
# Unraid template, an OMV or Proxmox compose profile and a Synology
# Container Manager project are all metadata, so the image is the only
# carrier they have. `serve-ui --ui-root /ui/bundles --profile=<name>`
# then serves the right bridge from the identical binary, which is what
# section 3.7 requires.
#
# Five, not seven. `generic` is compiled into the binary already and
# duplicating it here would buy nothing; `ugos` is EPIC D's, and its UPK
# carries its own. Seven at roughly 347 KB each would be about 2.4 MB
# against a gated 5% image budget of about 2.15 MB, so "ship them all" was
# never available; five is 1.74 MB, which is inside it with room, and
# docs/perf/ records the measurement rather than the estimate.
RUN cd ui/shared && npm run build:bundles truenas unraid openmediavault proxmox synology

# LAST, and the order is load-bearing. `build:bundles` runs `npm run
# build` once per provider, so it leaves dist/ holding whichever provider
# it built last; this rebuild puts the generic bundle back, and that is
# the one the build-web stage embeds. Reversing these two lines would ship
# a canonical image whose compiled-in UI is a NAS provider's bridge, which
# nothing downstream would notice.
# distribution/packaging's TestTheCanonicalImageCarriesEachAdapterBundle
# fails the build if the order is swapped.
RUN cd ui/shared && npm run build

# ---- generic Web host builder ------------------------------------------
# apps/generic's own binary (issue #82/B4.1): imports core/service and
# apps/common (webhost + auth/local), and embeds the frontend stage's
# dist/ output. Reuses the exact same base image/toolchain settings as
# the core builder above for the same reproducibility reasons.
FROM --platform=$BUILDPLATFORM golang@sha256:ded31c68586d2e49e760acc2e65a884b23d032e9bbbed0ae0c55abd3fcaf4452 AS build-web

ENV CGO_ENABLED=0 \
    GOTOOLCHAIN=local \
    GOFLAGS=-mod=readonly

WORKDIR /src

# apps/generic's go.mod replaces core/ and apps/common/ with relative
# filesystem paths (../../core, ../common - see apps/generic/go.mod's own
# comment), so this layout preserves the repo's real relative directory
# structure under /src, unlike the core-only `build` stage above (which
# flattens core/ to /src's own root, since it never needs to resolve a
# sibling module by relative path).
COPY core/go.mod core/go.sum ./core/
COPY apps/common/go.mod apps/common/go.sum ./apps/common/
COPY apps/generic/go.mod apps/generic/go.sum ./apps/generic/
RUN --mount=type=cache,target=/go/pkg/mod \
    cd apps/generic && go mod download

COPY core/cmd/ ./core/cmd/
COPY core/internal/ ./core/internal/
COPY core/service/ ./core/service/
COPY core/migrations/ ./core/migrations/
COPY apps/common/ ./apps/common/
COPY apps/generic/ ./apps/generic/

# Overwrites apps/generic/webui/dist's checked-in placeholder (see that
# directory's own doc comment) with the real built frontend, before
# go:embed ever sees it.
COPY --from=frontend-build /src/ui/shared/dist/ ./apps/generic/webui/dist/

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=none

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    cd apps/generic && GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
        -trimpath \
        -buildvcs=false \
        -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
        -o /out/backup-manager-web \
        ./cmd/backup-manager-web

# ---- runtime ------------------------------------------------------------
# distroless static, nonroot variant: no shell, no package manager, no libc
# (neither binary needs any, see CGO_ENABLED=0 above), and no `rclone`
# binary on it or on anything the base image could have carried in. Pinned
# by digest for the same reproducibility reason as the builders. The
# nonroot variant already runs as uid/gid 65532 by default;
# container/compose.yaml overrides that at deploy time to match the
# UGREEN host's actual storage ownership (see "Non-root and the NAS
# uid/gid" in docs/deployment.md for why that has to be an override
# rather than a value baked in here).
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

COPY --from=build /out/backup-manager /backup-manager
COPY --from=build-web /out/backup-manager-web /backup-manager-web

# The licence, the notices and the machine-readable inventory the other
# two point at, at a fixed path a recipient can find without a shell
# (distroless has none): `docker cp <ctr>:/licenses .` or `docker export
# | tar -x licenses/`. Apache-2.0 §4(d) says a redistributed copy
# carries NOTICE, and NOTICE is where the MPL-2.0 §3.2 source offer for
# go-cleanhttp and go-retryablehttp lives, so an image without these
# reached nobody with either.
#
# All three come from the build context, not from a builder stage, so
# they are the checked-in files at the commit this image was built from.
# .dockerignore excludes provenance/ wholesale, because that directory
# describes the image rather than building it, so the inventory needs the
# one exception in there and that exception is not decoration: without it
# this COPY fails the build with "failed to compute cache key ... not
# found", which is loud, and distribution/packaging's
# TestTheImageCarriesTheLicenceMaterials says the same thing earlier and
# names the pattern that did it.
#
# Two checks stand behind these lines. TestTheImageCarriesTheLicenceMaterials
# reads this stage and fails if a COPY goes or moves; and
# apps/generic/tests/dockercli's TestTheBuiltImageCarriesTheLicenceMaterials
# builds this file and copies /licenses back out of the result, which is
# the only one of the two that proves an image rather than a Dockerfile.
COPY LICENSE /licenses/LICENSE
COPY NOTICE /licenses/NOTICE
COPY provenance/third-party-licenses.json /licenses/third-party-licenses.json

# What `docker inspect` answers, for a recipient who has the image and no
# shell in it. The OCI annotation names the licence; the path label says
# where its text and the notices actually are, which an id on its own
# does not. Both values are declared in
# distribution/packaging/compliance.json under distribution.licenceDelivery
# and checked against these lines.
LABEL org.opencontainers.image.licenses="Apache-2.0" \
      com.iasbuilt.backupmanager.licenses.path="/licenses"

# The per-provider UI bundles (issue #180). Read-only content, served by
# `serve-ui --ui-root /ui/bundles`, which resolves <root>/<profile>. Each
# converted distribution adapter sets UI_ROOT to this path and selects its
# own profile, so an operator installing through the TrueNAS catalog,
# Community Applications, an OMV or Proxmox compose profile or a Synology
# Container Manager project gets that platform's bridge instead of being
# told this is a generic Docker deployment.
COPY --from=frontend-build /src/ui/shared/dist-bundles/ /ui/bundles/

# Documentary only - EXPOSE does not itself publish anything (see
# container/compose.yaml's own `ports:` for what actually does): this is
# `/backup-manager-web serve`'s own default --listen port, self-documented
# on the image for `docker inspect`/`docker port`/tooling. A headless-only
# deployment that never runs `serve` simply never binds it.
EXPOSE 8080

# Deliberately NO ENTRYPOINT: this image now ships two binaries, and an
# ENTRYPOINT is a fixed prefix Docker prepends to whatever `command:` (or
# `docker run <image> ...`) supplies, so a single ENTRYPOINT could only
# ever name one of the two - exactly the trap an earlier version of this
# file fell into (`ENTRYPOINT ["/backup-manager"]` plus `command:
# ["/backup-manager-web", "serve"]` resolves to running
# "/backup-manager /backup-manager-web serve", not what anyone intended).
# Every `command:` in container/compose.yaml and every `docker run`
# example in docs/deployment.md therefore names its binary explicitly by
# full path (`/backup-manager-web serve`, `/backup-manager daemon`,
# `/backup-manager version`, ...). This image's own default CMD stays
# unset for the same reason: there is no single default that would be
# right for both binaries, so a bare `docker run <image>` with no command
# fails immediately with "no command specified" - a loud, safe failure,
# not a silent no-op or an ambiguous guess.

# `backup-manager status` (issue #26, FR-24) exits 0 only when every
# configured backup set reports HEALTHY, and non-zero on DEGRADED, STALE or
# FAILING (see core/cmd/backup-manager/status.go's own doc for the exact
# contract). This is the real backup-freshness signal FR-24's health states
# are meant to be read through - not process liveness, which is what the
# previous `version`-based HEALTHCHECK could only ever answer. Verified
# directly (see docs/deployment.md and apps/generic/tests/dockercli): a container
# whose one backup set is DEGRADED now reports Docker health "unhealthy",
# not "healthy". This runs against the plain `/backup-manager` binary
# regardless of which command the container's own main process
# (`/backup-manager-web serve`, or a headless `/backup-manager daemon`) is
# running: `status` is a fresh, read-only check against the same shared
# state database file either way.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD ["/backup-manager", "status"]
