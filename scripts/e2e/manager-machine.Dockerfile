# The MANAGER machine for the Go machine tier (#451): the box rclone-manager
# runs on, playing the NAS, with the toolchain to run core/tests inside it
# and a docker client to reach the daemon that stands the other machines up.
#
# The Go version tracks core/go.mod. scripts/e2e/run-machine-tier.sh reads
# the go directive and passes it as a build argument rather than guessing,
# so a bumped directive fails loudly here instead of silently compiling the
# tier against an older toolchain.
#
# The docker client is copied from the official CLI image rather than
# installed from a distribution package. Debian's docker.io on trixie
# installs a runtime without /usr/bin/docker, so the manager came up unable
# to run a single command against the socket, which is a ten-minute
# discovery at `go test` time and a one-line one here. Copying from
# docker:<version>-cli also gets the architecture right for free.
#
# The buildx plugin comes with it, and it is not optional. Without it the
# CLI falls back to the legacy builder, which does not understand
# --progress=plain, and the harness's own image build is watched through
# exactly that flag (#309): the build fails with "unknown flag: --progress"
# and no machine comes up at all. That is what happened on the first full
# run inside a manager container.
#
# git is here because `go test` shells out to it for VCS stamping,
# openssh-client because the machine tier generates its key material with
# ssh-keygen and records host keys with ssh-keyscan exactly as it does on a
# developer's machine, and netcat because the connection-cap probe uses it.
# See run-machine-tier.sh's header for why this container gets a socket and
# two-machine-backup.sh's manager deliberately does not.
ARG GO_VERSION
FROM docker:28-cli AS cli

FROM golang:${GO_VERSION}
COPY --from=cli /usr/local/bin/docker /usr/local/bin/docker
COPY --from=cli /usr/local/libexec/docker/cli-plugins/docker-buildx /usr/local/libexec/docker/cli-plugins/docker-buildx
RUN apt-get update \
 && apt-get install -y --no-install-recommends git openssh-client netcat-openbsd iproute2 \
 && rm -rf /var/lib/apt/lists/*
