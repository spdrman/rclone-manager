# The CLIENT machine: a browser, and the test runner that drives it, both
# inside the container.
#
# This is the third machine in scripts/e2e/three-machine-web-ui.sh. It is
# not "Playwright on the developer's Mac talking to a remote Chromium over
# CDP", and it is not a bare browser something else attaches to. `npx
# playwright test` runs HERE, so the runner, the browser and the page all
# share one network position: the edge network, where the only thing
# reachable is the product's own UI container. A spec that reaches the
# engine directly cannot pass by accident, because from in here there is no
# route to it.
#
# # Why the official image
#
# Chromium in a container is a pile of shared libraries (nss, atk, cups,
# pango, libdrm, and a dozen more) that no base image carries by default,
# and the failure when one is missing is a browser that exits during launch
# with a linker error. mcr.microsoft.com/playwright ships that dependency
# set and the matching browser builds, maintained by the people who decide
# what the browser build needs. Assembling the same thing from ubuntu:24.04
# and apt-get would be a second, worse copy of it that goes stale silently.
#
# # Pinned by digest, like every other base in this repository
#
# container/Dockerfile pins golang, node and distroless by digest, so this
# does too. A floating v1.62.1-noble is a tag somebody else can move, and
# test infrastructure that changes underneath a green run is the same
# problem as a product image that does, one layer down. The digest below is
# the OCI INDEX digest, not one platform's manifest, so it resolves on
# amd64 and arm64 alike (this was written on an arm64 Mac and CI's runners
# are amd64).
#
# Moving it is two commands and a commit:
#
#   docker buildx imagetools inspect mcr.microsoft.com/playwright:vX.Y.Z-noble
#   # take the top-level "Digest:" line, not one of the per-platform ones
#
# # The version tracks the tests repository's lockfile, deliberately
#
# PLAYWRIGHT_VERSION and the image tag are the same number, and that number
# is whatever spdrman/rclone-manager-tests has in
# suites/web-ui/package-lock.json. It has to be: the browsers live in the
# image at /ms-playwright under a build id the npm package computes, so a
# runner from one version looking for the browsers of another finds
# nothing and says "Executable doesn't exist", which reads like a broken
# image rather than a version skew. Bump both together or neither.
ARG PLAYWRIGHT_IMAGE=mcr.microsoft.com/playwright:v1.62.1-noble@sha256:dcc5531e97840b9b5e794f2814476b21571c5124a3fca2267d73041f56e7580e
FROM ${PLAYWRIGHT_IMAGE}

ARG PLAYWRIGHT_VERSION=1.62.1

# The browsers are already in the image at /ms-playwright, so the npm
# postinstall must not go and fetch a second copy: it would double the
# layer, and on a machine with no route to Playwright's CDN it would fail
# the build for something already present.
ENV PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1 \
    PLAYWRIGHT_BROWSERS_PATH=/ms-playwright \
    CI=1

# /suite is where a mounted suite lands at run time, and node_modules is
# installed HERE rather than mounted from the host on purpose. The tests
# repository's own node_modules is built for the developer's machine
# (darwin/arm64 binaries for esbuild and friends), and mounting that into a
# linux container is a class of failure with no useful error attached to
# it. The harness mounts the suite source over /suite and puts an anonymous
# volume over /suite/node_modules, which Docker seeds from this layer, so
# the specs come from the checkout and the runtime comes from the image.
WORKDIR /suite
RUN npm init -y >/dev/null 2>&1 \
 && npm install --no-audit --no-fund --save-exact "@playwright/test@${PLAYWRIGHT_VERSION}" \
 && npx playwright --version

# The built-in check, for when there is no suite to mount. It proves this
# stack rather than any spec: the login page renders, a generated
# administrator can sign in, the seeded backup set is listed, and the
# Activity page (the one that failed on a real NAS while the server
# answered 200 in 20ms) renders without a console error or an unhandled
# rejection.
COPY web-ui-smoke.mjs /suite/smoke.mjs

# a+rwX, and it is not laziness. The harness runs this container as the
# INVOKING user's uid so Playwright's traces and screenshots land on the
# host owned by the person who has to read them, and that uid has no
# account in this image. Playwright writes into node_modules/.cache while
# transpiling the config and the specs, so the tree it writes into has to
# be writable by a uid this image has never heard of.
RUN chmod -R a+rwX /suite

# No ENTRYPOINT and no CMD: the harness passes the command, because the two
# things this container does (run a mounted suite, run the built-in check)
# are different command lines and neither is more default than the other.
