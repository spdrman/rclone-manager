# syntax=docker/dockerfile:1.7

# backup-manager container image (A3.9).
#
# Two stages: a builder that compiles the single Go binary (manager logic
# plus the embedded rclone packages, see core/internal/transport/rclone),
# and a distroless runtime that carries nothing but that binary and the TLS
# root store. There is no `rclone` executable anywhere in this image: rclone
# is consumed as a Go module (core/go.mod pins github.com/rclone/rclone),
# imported as packages, and compiled straight into /backup-manager. See
# docs/deployment.md for how that claim is verified against the built
# image, rather than just asserted here.
#
# The engine lives in its own core/ Go module (docs/EPIC-B-multi-nas.md
# §7 — B1.1/#106 drew this boundary): everything this build needs comes
# from core/, never from apps/ or ui/.
#
# cmd/backup-manager currently only implements a `version` subcommand
# (execution modes and the rest of the CLI are separate, still-open issues).
# This image and container/compose.yaml are deliberately packaging ahead of
# that work: they define the shape a real deployment will run in
# (volumes, uid/gid, restart policy, health check) without pretending a
# daemon exists yet. Search this file and container/compose.yaml for
# "TODO(daemon)" for the two spots that need to change once it does.

# ---- builder ----------------------------------------------------------
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
# -ldflags -s -w: drop the symbol table and DWARF info; cmd/backup-manager
# only exposes `version` today, there is no debugger-driven use case that
# needs them in the shipped image, and it shrinks the minimal runtime image.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
        -trimpath \
        -buildvcs=false \
        -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
        -o /out/backup-manager \
        ./cmd/backup-manager

# ---- runtime ------------------------------------------------------------
# distroless static, nonroot variant: no shell, no package manager, no libc
# (the binary needs none, see CGO_ENABLED=0 above), and no `rclone` binary
# on it or on anything the base image could have carried in. Pinned by
# digest for the same reproducibility reason as the builder. The nonroot
# variant already runs as uid/gid 65532 by default; container/compose.yaml
# overrides that at deploy time to match the UGREEN host's actual storage
# ownership (see "Non-root and the NAS uid/gid" in docs/deployment.md for
# why that has to be an override rather than a value baked in here).
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

COPY --from=build /out/backup-manager /backup-manager

ENTRYPOINT ["/backup-manager"]
# TODO(daemon): once `backup-manager run` / `daemon` exist, this is where a
# real deployment's long-running command goes; today the only subcommand
# that exists is `version`, so there is nothing to default CMD to that
# would actually stay up. container/compose.yaml supplies `command:
# ["version"]` explicitly for the same reason.

# `backup-manager status` (issue #26) doesn't exist yet, so this can't ask
# "is the service healthy" the way FR-24's health states are meant to be
# read. It asks the narrower question that's actually answerable today: can
# the binary still start up and exit 0. That's process liveness, not
# backup freshness (the failure-safety invariants are explicit that those
# are not the same fact) — it exists so the container has *a* health check
# now, not so it substitutes for one.
#
# TODO(#26): replace this CMD with `["/backup-manager", "status"]` (or
# whatever flag makes it exit non-zero on DEGRADED/STALE/FAILING) the day
# that subcommand ships. Until then, a HEALTHCHECK that always reports
# healthy would be actively misleading, so this one is left checking the
# one true thing available: the binary runs.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD ["/backup-manager", "version"]
