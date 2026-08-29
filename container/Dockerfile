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
# cmd/backup-manager now implements every execution mode FR-1/FR-24 call
# for: `run`, `daemon`, `check`, `status`, `sources`, `artifacts`, `fetch`,
# `retention`, `reconcile`, `validate` and `version` (issue #82/B4.1 is what
# caught this image's own CMD/HEALTHCHECK up to that, after EPIC A shipped
# this file ahead of the daemon existing at all).

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
# `run`/`daemon`/`status`/`check`/`sources`/`artifacts`/`fetch`/`retention`/
# `reconcile`/`validate`/`version` all exist now (issue #82/B4.1). This
# image's own default CMD stays deliberately unset: container/compose.yaml
# and every `docker run` example in docs/deployment.md say explicitly which
# one they want (`daemon` for the long-running headless case, `version` for
# a one-shot smoke check, `status` for the healthcheck below), rather than
# relying on a default that would silently mean something different from
# what the caller intended.

# `backup-manager status` (issue #26, FR-24) exits 0 only when every
# configured backup set reports HEALTHY, and non-zero on DEGRADED, STALE or
# FAILING (see cmd/backup-manager/status.go's own doc for the exact
# contract). This is the real backup-freshness signal FR-24's health states
# are meant to be read through — not process liveness, which is what the
# previous `version`-based HEALTHCHECK could only ever answer. Verified
# directly (see docs/deployment.md and core/tests/dockercli): a container
# whose one backup set is DEGRADED now reports Docker health "unhealthy",
# not "healthy".
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD ["/backup-manager", "status"]
