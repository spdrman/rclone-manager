# The gate's toolchain, pinned, so a run stops depending on what the
# machine it runs on happens to have installed.
#
# This exists because of a specific failure rather than a preference. A
# macOS Command Line Tools update landed mid-session and broke every cgo
# link on the development machine: xcrun selects the NEWEST SDK it can
# find, that became MacOSX27.0.sdk, and the installed linker (ld-1267,
# from Command Line Tools 26.6) reads its .tbd files as "unknown
# architecture". Nothing in this repository was wrong, every run failed
# at `core/ go build`, and no amount of re-running would have fixed it.
# Three other runs the same evening died on a concurrent `go clean
# -cache`, on two other gate runs competing for the Docker VM, and on a
# workspace whose node_modules were not installed. A gate whose verdict
# depends on what somebody's laptop updated overnight is not a gate.
#
# Everything the gate shells out to is here and pinned: go, golangci-lint
# at the version .github/workflows/ci.yml installs, node and npm at the
# major that workflow uses, the docker CLI and the compose plugin.
#
# The DAEMON is deliberately not here. scripts/ci-docker.sh explains why
# this talks to the host's daemon through a mounted socket instead of
# starting one of its own: the two-machine proof needs a privileged
# container, and a sibling on the host daemon is exactly what it already
# gets today.

ARG GO_VERSION=1.27
FROM golang:${GO_VERSION}-bookworm

# Restated from .github/workflows/ci.yml rather than fetched as latest.
# A lint gate that changes underneath you is a gate that goes red for a
# reason nobody in the repository caused.
ARG GOLANGCI_LINT_VERSION=v2.13.2

# node and npm copied out of the official image rather than installed
# from a distribution repository, so this and CI are the same build and
# not merely the same version number.
COPY --from=node:22-bookworm-slim /usr/local/bin/node /usr/local/bin/node
COPY --from=node:22-bookworm-slim /usr/local/lib/node_modules /usr/local/lib/node_modules

# The client and the two plugins the gate invokes. No daemon, no dind.
COPY --from=docker:28-cli /usr/local/bin/docker /usr/local/bin/docker
COPY --from=docker:28-cli /usr/local/libexec/docker/cli-plugins/docker-compose /usr/local/libexec/docker/cli-plugins/docker-compose
COPY --from=docker:28-cli /usr/local/libexec/docker/cli-plugins/docker-buildx /usr/local/libexec/docker/cli-plugins/docker-buildx

RUN set -eux; \
    ln -s ../lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm; \
    ln -s ../lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx; \
    apt-get update; \
    apt-get install -y --no-install-recommends python3 jq; \
    rm -rf /var/lib/apt/lists/*; \
    go install "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@${GOLANGCI_LINT_VERSION}"; \
    go clean -cache -modcache; \
    go version; node --version; npm --version; docker --version; \
    docker compose version; golangci-lint --version; python3 --version

# Every checkout this runs against is bind-mounted from the host and is
# owned by the host user, not by root. Without this git refuses it as
# dubious ownership and the gate's first git call fails. Set in the image
# rather than in the user's own config, which this must never write to.
RUN git config --system --add safe.directory '*'

# A writable home for whatever uid the wrapper runs as, which is the
# HOST's uid rather than root's.
#
# Running as root was the obvious choice and it silently cost a test.
# TestArchivingIsInsideTheRefusalContract sets a directory unwritable and
# checks the installer refuses instead of raising, and root ignores the
# mode, so the case cannot be set up and skips itself. It said so plainly
# in the output and it would have gone on saying so for ever, which is
# the kind of coverage that disappears without anyone deciding to drop
# it. A non-root uid gets the assertion back.
#
# 0777 rather than a chown to a fixed uid: the wrapper passes the host
# user's own uid, which differs between machines, and a named volume
# takes its permissions from the image directory it first shadows.
ENV HOME=/ci \
    GOPATH=/ci/go \
    GOCACHE=/ci/.cache/go-build \
    GOMODCACHE=/ci/go/pkg/mod \
    npm_config_cache=/ci/.npm \
    XDG_CACHE_HOME=/ci/.cache

# The uid the wrapper will run as, which is the host user's, so that a
# file this creates on the bind mount belongs to the person who ran it.
#
# It needs a real passwd entry and not just a numeric --user, which is
# where the first attempt at this failed: ssh-keygen resolves the
# invoking uid and refuses with "No user exists for uid 501", so the
# installer's own credential tests error rather than run. Anything that
# calls getpwuid does the same, so the entry has to exist rather than the
# callers having to cope.
ARG CI_UID=1000
ARG CI_GID=1000
RUN set -eux; \
    if ! getent group "${CI_GID}" >/dev/null; then groupadd -g "${CI_GID}" ci; fi; \
    useradd -o -u "${CI_UID}" -g "${CI_GID}" -d /ci -s /bin/bash -M ci; \
    mkdir -p /ci/.cache/go-build /ci/go/pkg/mod /ci/.npm /ci/.cache/rclone-manager-tests-gate; \
    chown -R "${CI_UID}:${CI_GID}" /ci; \
    chmod -R 0775 /ci; \
    getent passwd "${CI_UID}"
