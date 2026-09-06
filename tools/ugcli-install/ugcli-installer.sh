#!/bin/sh
# Puts UGREEN's own `ugcli` on a UGOS Pro NAS, at whatever version UGREEN has
# published most recently.
#
# `ugcli` is the vendor's packaging tool, the thing `ugcli pack --build <n>`
# turns a directory into a `.upk` with, so nothing in the UGOS packaging story
# can be tried by hand until it is on the machine. There is no repository to
# apt-get it from and no artifact of it in this checkout, which leaves two ways
# to get it there. The committed `.deb` under dpkg/ is the repeatable one: its
# bytes were reviewed and its SHA-256 is recorded beside it, and it stays
# pinned for exactly that reason. This script is the current one, and it is
# knowingly the weaker of the two, because it installs an artifact nobody here
# has looked at. README.md sets out which to reach for; that choice is the
# whole reason both exist.
#
# One fact about the endpoint shapes most of what follows, and the
# configuration block below sets out the evidence for it: UGREEN publishes no
# `latest` pointer, no manifest and no directory listing, so "newest" cannot be
# asked for and has to be found. The script walks version numbers upward from
# the last one it was verified against until the server stops answering. That
# is where the version floor comes from, why probing carries on a little past
# the first 404 rather than stopping dead, and why a 404 and an unreachable
# server are handled differently: a miss is evidence and a timeout is not, and
# reading a timeout as "nothing newer" would pin the install to the floor while
# calling it the latest.
#
# The remaining length is an appliance being an unforgiving place to install
# into, and each decision here is a reaction to that rather than a style.
# /bin/sh with no bashisms, because UGOS does not promise bash. sudo asked for
# at the point something first needs root instead of up front, so a person
# reading the prompt can see what it is about to be spent on. Only the system
# packages actually found missing get installed, because reinstalling a working
# dependency on a NAS is a good way to stop it being one. And every failure the
# script cannot fix ends in printed manual recovery steps, because whoever hits
# one is at the end of an SSH session with no other way in.

set -eu

# ============================================================
# Configuration
# ============================================================

# UGREEN publishes no "latest" pointer, no manifest and no directory listing.
# Checked on 2026-08-29:
#
#   /pro/ugcli/download/ugcli-latest-linux-amd64        404
#   /pro/ugcli/version, /version.json, /manifest.json   404
#   /pro/ugcli/ and /pro/ugcli/download/                200, empty bodies
#
# What the endpoint DOES give is an enumerable, verifiable URL scheme:
#
#   ugcli-v1.1.0.12-linux-amd64   200
#   ugcli-v1.1.0.13-linux-amd64   200
#   ugcli-v1.1.0.14-linux-amd64   404
#
# So "latest" is discovered by walking the version upward from a known-good
# floor until the endpoint stops answering, rather than by asking for it. That
# is the only mechanism the vendor exposes.
#
# UGCLI_VERSION_FLOOR is the newest version this script has been verified
# against. It is a starting point and a fallback, never a ceiling: discovery
# only moves upward from it, and if discovery cannot run at all (no network, a
# WAF in the way, no downloader present) the install proceeds with the floor
# rather than failing, because a pinned install beats no install.
#
# To pin deliberately, set UGCLI_VERSION in the environment:
#
#   UGCLI_VERSION=1.1.0.13 ./ugcli-installer.sh
#
UGCLI_VERSION_FLOOR="1.1.0.13"
UGCLI_VERSION="${UGCLI_VERSION:-}"

UGCLI_BASE_URL="https://osswaf.ugnas.com/pro/ugcli/download"
UGCLI_INSTALL="/usr/local/bin/ugcli"

# How far to keep probing past the last version that answered. UGREEN has
# published consecutive patch numbers so far, but a withdrawn build would leave
# a hole, and stopping at the first 404 would then hide every later release.
UGCLI_PROBE_MISSES=3

GET_PIP_URL="https://bootstrap.pypa.io/get-pip.py"

WORKDIR=""

# ============================================================
# Logging helpers
# ============================================================

log()
{
    printf '%s\n' "$*"
}

warn()
{
    printf 'WARNING: %s\n' "$*" >&2
}

die()
{
    printf '\nERROR: %s\n' "$*" >&2
    exit 1
}

has_cmd()
{
    command -v "$1" >/dev/null 2>&1
}

# ============================================================
# Debian package detection
# ============================================================

pkg_installed()
{
    has_cmd dpkg-query || return 1

    dpkg-query \
        -W \
        -f='${Status}' \
        "$1" 2>/dev/null \
        | grep -q '^install ok installed$'
}

# ============================================================
# sudo handling
#
# We don't ask for sudo until something actually needs
# system-level modification.
# ============================================================

SUDO=""

ensure_sudo()
{
    if [ "$(id -u)" -eq 0 ]; then
        SUDO=""
        return 0
    fi

    if ! has_cmd sudo; then
        die "sudo is required.

MANUAL ACTION REQUIRED:
Enable sudo for this UGOS administrator account, or run this
installer as root."
    fi

    if ! sudo -v; then
        die "sudo authentication failed.

MANUAL ACTION REQUIRED:
Run this installer using a UGOS administrator account that has
sudo access."
    fi

    SUDO="sudo"
}

# ============================================================
# Current script path
#
# Used to make sure cleanup never deletes the script that is
# currently executing.
# ============================================================

CURRENT_SCRIPT="$0"

if has_cmd readlink; then
    CURRENT_SCRIPT="$(
        readlink -f "$0" 2>/dev/null ||
        printf '%s' "$0"
    )"
fi

# ============================================================
# Legacy cleanup
#
# Earlier versions of this installer used a fixed file:
#
#     /tmp/install-ugcli.sh
#
# That caused the Permission denied issue you encountered when
# another user/root owned the stale file.
#
# This installer NEVER uses that fixed filename.
# We clean it only if it clearly looks like one of our old
# UGREEN/ugcli installer scripts.
# ============================================================

cleanup_legacy_installer()
{
    LEGACY="/tmp/install-ugcli.sh"

    [ -e "$LEGACY" ] || return 0

    RESOLVED="$LEGACY"

    if has_cmd readlink; then
        RESOLVED="$(
            readlink -f "$LEGACY" 2>/dev/null ||
            printf '%s' "$LEGACY"
        )"
    fi

    # Never delete ourselves.
    if [ "$RESOLVED" = "$CURRENT_SCRIPT" ]; then
        return 0
    fi

    log "Found legacy installer artifact:"
    log "  $LEGACY"

    # Only delete it automatically if it looks like our
    # previous UGREEN ugcli installer.
    if grep -qi 'UGREEN' "$LEGACY" 2>/dev/null &&
       grep -qi 'ugcli' "$LEGACY" 2>/dev/null
    then
        if rm -f "$LEGACY" 2>/dev/null; then
            log "  Cleaned."
            return 0
        fi

        if has_cmd sudo; then
            if sudo -v >/dev/null 2>&1 &&
               sudo rm -f "$LEGACY"
            then
                log "  Cleaned with sudo."
                return 0
            fi
        fi

        warn "Could not remove $LEGACY."
        warn "It will NOT interfere with this run because this installer uses a unique temporary directory."

    else
        warn "$LEGACY exists but does not clearly match our old installer."
        warn "Leaving it untouched."
    fi
}

# ============================================================
# Unique working directory
#
# Every run gets its own directory.
#
# /tmp/ugcli-installer.xxxxxx
#
# If /tmp cannot be written, fall back to ~/.cache.
# ============================================================

make_workdir()
{
    BASE="${TMPDIR:-/tmp}"

    if WORKDIR="$(
        mktemp -d "${BASE%/}/ugcli-installer.XXXXXX" \
        2>/dev/null
    )"
    then
        return 0
    fi

    FALLBACK="$HOME/.cache"

    mkdir -p "$FALLBACK"

    WORKDIR="$(
        mktemp -d \
        "$FALLBACK/ugcli-installer.XXXXXX"
    )" || die "Could not create a temporary working directory."
}

cleanup_workdir()
{
    if [ -n "${WORKDIR:-}" ] &&
       [ -d "$WORKDIR" ]
    then
        rm -rf "$WORKDIR" 2>/dev/null || true
    fi
}

# ============================================================
# APT health check
# ============================================================

apt_health_check()
{
    apt-get check \
        >/dev/null \
        2>"$WORKDIR/apt-check.log"
}

# ============================================================
# Display safe manual recovery instructions
# ============================================================

print_apt_manual_help()
{
    PACKAGES="$1"

    log ""
    log "========================================"
    log " MANUAL ACTION REQUIRED"
    log "========================================"
    log ""
    log "UGOS currently has unresolved APT dependencies."
    log ""
    log "The installer needs these missing package(s):"
    log "  $PACKAGES"
    log ""
    log "The installer WILL NOT run:"
    log ""
    log "  apt --fix-broken install"
    log ""
    log "automatically because that operation can remove or replace"
    log "UGOS vendor-managed packages."
    log ""
    log "Inspect the proposed repair first:"
    log ""
    log "  sudo apt-get check"
    log ""
    log "  sudo apt-get --fix-broken install --simulate"
    log ""

    if [ -s "$WORKDIR/apt-check.log" ]; then
        log "APT reported:"
        log ""

        sed 's/^/  /' \
            "$WORKDIR/apt-check.log" \
            | head -n 30
    fi
}

# ============================================================
# Safe package installation
#
# Process:
#
#   1. Only gets called for packages that are actually missing.
#   2. Check existing APT health.
#   3. Simulate exact transaction.
#   4. If simulation fails, update indexes once.
#   5. Simulate again.
#   6. Only then perform actual installation.
#
# We NEVER automatically run --fix-broken.
# ============================================================

install_system_packages()
{
    PACKAGES="$1"

    [ -n "$PACKAGES" ] || return 0

    if ! has_cmd apt-get; then
        log ""
        log "========================================"
        log " MANUAL ACTION REQUIRED"
        log "========================================"
        log ""
        log "apt-get is unavailable."
        log ""
        log "Install these dependencies manually:"
        log "  $PACKAGES"
        exit 1
    fi

    log ""
    log "Checking APT health..."

    if ! apt_health_check; then
        print_apt_manual_help "$PACKAGES"
        exit 1
    fi

    log "[OK] APT dependency state is healthy."
    log ""
    log "Simulating installation of:"
    log "  $PACKAGES"

    if apt-get \
        -s \
        install \
        -y \
        --no-install-recommends \
        $PACKAGES \
        >/dev/null \
        2>"$WORKDIR/apt-sim.log"
    then
        log "[OK] Package transaction simulation succeeded."

    else

        log ""
        log "Initial package simulation failed."
        log "Refreshing package indexes once..."

        ensure_sudo

        if ! $SUDO apt-get update; then
            log ""
            log "========================================"
            log " MANUAL ACTION REQUIRED"
            log "========================================"
            log ""
            log "apt-get update failed."
            log ""
            log "Required packages:"
            log "  $PACKAGES"
            log ""
            log "Repair the configured UGOS/Debian repositories manually"
            log "and rerun this installer."
            exit 1
        fi

        log ""
        log "Rechecking APT health..."

        if ! apt_health_check; then
            print_apt_manual_help "$PACKAGES"
            exit 1
        fi

        log ""
        log "Retrying package transaction simulation..."

        if ! apt-get \
            -s \
            install \
            -y \
            --no-install-recommends \
            $PACKAGES \
            >/dev/null \
            2>"$WORKDIR/apt-sim.log"
        then
            log ""
            log "========================================"
            log " MANUAL ACTION REQUIRED"
            log "========================================"
            log ""
            log "APT cannot safely resolve:"
            log "  $PACKAGES"
            log ""

            if [ -s "$WORKDIR/apt-sim.log" ]; then
                log "APT simulation reported:"
                log ""

                sed 's/^/  /' \
                    "$WORKDIR/apt-sim.log" \
                    | head -n 40
            fi

            exit 1
        fi
    fi

    ensure_sudo

    log ""
    log "Installing ONLY missing package(s):"
    log "  $PACKAGES"
    log ""

    if ! $SUDO apt-get \
        install \
        -y \
        --no-install-recommends \
        $PACKAGES
    then
        log ""
        log "========================================"
        log " MANUAL ACTION REQUIRED"
        log "========================================"
        log ""
        log "APT failed while installing:"
        log "  $PACKAGES"
        log ""
        log "No automatic APT repair was attempted."
        exit 1
    fi
}

# ============================================================
# Download helper
#
# Preference:
#
#   1. Python HTTPS
#   2. curl
#   3. wget
#
# ============================================================

download_file()
{
    URL="$1"
    DEST="$2"

    rm -f "$DEST"

    if has_cmd python3 &&
       python3 -c \
           'import ssl, urllib.request' \
           >/dev/null 2>&1
    then

        log "Downloading with Python:"
        log "  $URL"

        python3 - "$URL" "$DEST" <<'PY'
import os
import sys
import urllib.request

url = sys.argv[1]
dest = sys.argv[2]

request = urllib.request.Request(
    url,
    headers={
        "User-Agent": "UGREEN-ugcli-installer/3.0"
    },
)

try:
    with urllib.request.urlopen(
        request,
        timeout=60,
    ) as response:

        with open(dest, "wb") as output:

            while True:
                chunk = response.read(1024 * 1024)

                if not chunk:
                    break

                output.write(chunk)

except Exception as exc:

    try:
        os.remove(dest)
    except FileNotFoundError:
        pass

    raise SystemExit(
        f"Download failed: {exc}"
    )

if not os.path.isfile(dest):
    raise SystemExit(
        "Download file was not created."
    )

if os.path.getsize(dest) == 0:
    raise SystemExit(
        "Downloaded file is empty."
    )

print(
    f"Downloaded {os.path.getsize(dest)} bytes."
)
PY

    elif has_cmd curl; then

        log "Downloading with curl:"
        log "  $URL"

        curl \
            --fail \
            --location \
            --show-error \
            --silent \
            --output "$DEST" \
            "$URL"

    elif has_cmd wget; then

        log "Downloading with wget:"
        log "  $URL"

        wget \
            -q \
            -O "$DEST" \
            "$URL"

    else

        log ""
        log "========================================"
        log " MANUAL ACTION REQUIRED"
        log "========================================"
        log ""
        log "No HTTPS downloader is available."
        log ""
        log "Install at least one of:"
        log ""
        log "  python3 with SSL support"
        log "  curl"
        log "  wget"
        exit 1
    fi

    if [ ! -s "$DEST" ]; then
        die "Download failed or produced an empty file:
$DEST"
    fi
}

# ============================================================
# Version discovery
#
# UGREEN exposes no manifest, so the only way to learn what
# exists is to ask for each candidate. These helpers do HEAD
# requests through whichever downloader is present.
# ============================================================

ugcli_url()
{
    printf '%s/ugcli-v%s-linux-amd64' \
        "$UGCLI_BASE_URL" \
        "$1"
}

# Exit status:
#   0  published
#   1  definitely not published (a real 404)
#   2  could not tell
#
# The distinction between 1 and 2 is the important one. Treating
# "could not tell" as "not published" would let one network blip
# silently pin the install to the floor and call it the latest.
ugcli_version_exists()
{
    URL="$(ugcli_url "$1")"

    if has_cmd python3 &&
       python3 -c \
           'import ssl, urllib.request' \
           >/dev/null 2>&1
    then

        python3 - "$URL" <<'PYHEAD' >/dev/null 2>&1
import sys
import urllib.error
import urllib.request

request = urllib.request.Request(
    sys.argv[1],
    method="HEAD",
    headers={
        "User-Agent": "UGREEN-ugcli-installer/3.0"
    },
)

try:
    with urllib.request.urlopen(
        request,
        timeout=20,
    ) as response:
        sys.exit(0 if response.status == 200 else 2)

except urllib.error.HTTPError as err:
    sys.exit(1 if err.code == 404 else 2)

except Exception:
    sys.exit(2)
PYHEAD
        return $?

    elif has_cmd curl; then

        CODE="$(
            curl \
                --silent \
                --head \
                --max-time 20 \
                --output /dev/null \
                --write-out '%{http_code}' \
                "$URL" 2>/dev/null
        )" || return 2

        case "$CODE" in
            200) return 0 ;;
            404) return 1 ;;
            *)   return 2 ;;
        esac

    elif has_cmd wget; then

        if wget \
            --spider \
            --quiet \
            --timeout=20 \
            "$URL"
        then
            return 0
        fi

        # wget --spider cannot separate a 404 from a transport
        # failure without parsing its output, so anything that is
        # not a success counts as "could not tell" and stops the
        # walk rather than moving it.
        return 2

    else
        return 2
    fi
}

# Bumps one component of a dotted version, zeroing everything
# below it. 1.1.0.13 field 3 -> 1.1.1.0
ugcli_bump()
{
    printf '%s' "$1" |
    awk -F. -v f="$2" '
        BEGIN { OFS = "." }
        {
            $f = $f + 1
            for (i = f + 1; i <= NF; i++) {
                $i = 0
            }
            print
        }
    '
}

# Takes one step on a single component. Prints the newer version
# if one is published, otherwise prints its input unchanged, so
# the caller can loop until it stops moving.
ugcli_probe_field()
{
    CURRENT="$1"
    FIELD="$2"

    TRY="$CURRENT"
    MISS=0

    while [ "$MISS" -lt "$UGCLI_PROBE_MISSES" ]; do

        TRY="$(ugcli_bump "$TRY" "$FIELD")"

        [ -n "$TRY" ] || break

        # `|| RESULT=$?` rather than a bare call: this script runs
        # under `set -e`, and a 404 is a return of 1, so a bare call
        # would end the installer at the first version that does not
        # exist, which is every probe past the newest one.
        RESULT=0
        ugcli_version_exists "$TRY" || RESULT=$?

        if [ "$RESULT" -eq 0 ]; then
            printf '%s' "$TRY"
            return 0
        fi

        if [ "$RESULT" -eq 2 ]; then
            break
        fi

        MISS=$((MISS + 1))

    done

    printf '%s' "$CURRENT"
}

resolve_ugcli_version()
{
    if [ -n "$UGCLI_VERSION" ]; then

        log "Version pinned by UGCLI_VERSION:"
        log "  $UGCLI_VERSION"

        PINNED_RESULT=0
        ugcli_version_exists "$UGCLI_VERSION" || PINNED_RESULT=$?

        case $PINNED_RESULT in
            0) log "[OK] UGREEN publishes that version." ;;
            1) warn "UGREEN does not publish ugcli $UGCLI_VERSION. Continuing because you asked for it explicitly." ;;
            *) warn "Could not confirm ugcli $UGCLI_VERSION is published. Continuing." ;;
        esac

        return 0
    fi

    log "Looking for the newest published ugcli..."
    log "  walking up from the last verified version: $UGCLI_VERSION_FLOOR"

    BEST="$UGCLI_VERSION_FLOOR"

    # Patch first, then minor, then major. Each pass restarts the
    # ones below it, so 1.1.0.13 -> 1.1.1.0 -> 1.1.1.7 is reachable
    # rather than stopping at the first component that moves.
    for FIELD in 4 3 2 1; do

        while : ; do

            NEXT="$(ugcli_probe_field "$BEST" "$FIELD")"

            [ "$NEXT" != "$BEST" ] || break

            BEST="$NEXT"
            log "  found $BEST"

        done

    done

    UGCLI_VERSION="$BEST"

    if [ "$UGCLI_VERSION" = "$UGCLI_VERSION_FLOOR" ]; then
        log "[OK] $UGCLI_VERSION is the newest published version."
    else
        log "[OK] Newer than the floor: $UGCLI_VERSION_FLOOR -> $UGCLI_VERSION"
    fi
}

# ============================================================
# Python HTTPS verification
# ============================================================

verify_python_https()
{
    if python3 -c \
        'import ssl, urllib.request' \
        >/dev/null 2>&1
    then
        log "[OK] Python HTTPS/TLS support."
        return 0
    fi

    # We can still continue if another downloader exists.
    if has_cmd curl; then
        warn "Python HTTPS support is unavailable; curl will be used for downloads."
        return 0
    fi

    if has_cmd wget; then
        warn "Python HTTPS support is unavailable; wget will be used for downloads."
        return 0
    fi

    log ""
    log "========================================"
    log " MANUAL ACTION REQUIRED"
    log "========================================"
    log ""
    log "Python 3 exists but its SSL/HTTPS modules are unavailable."
    log ""
    log "You must either:"
    log ""
    log "  repair/reinstall Python 3 SSL support"
    log ""
    log "or install:"
    log ""
    log "  curl"
    log ""
    log "or:"
    log ""
    log "  wget"
    exit 1
}

# ============================================================
# Start
# ============================================================

log "========================================"
log " UGREEN UGCLI + Python Installer"
log "========================================"
log ""

# ============================================================
# Base environment dependency check
#
# These commands are fundamental enough that the installer
# cannot safely bootstrap itself without them.
# ============================================================

log "Checking base shell dependencies..."

BASE_MISSING=""

for CMD in \
    id \
    uname \
    mktemp \
    rm \
    mkdir \
    chmod \
    grep \
    sed \
    head \
    dirname
do

    if has_cmd "$CMD"; then
        :
    else
        BASE_MISSING="$BASE_MISSING $CMD"
    fi

done

if [ -n "$BASE_MISSING" ]; then

    log ""
    log "========================================"
    log " MANUAL ACTION REQUIRED"
    log "========================================"
    log ""
    log "The following fundamental shell utilities are missing:"
    log "  $BASE_MISSING"
    log ""
    log "Install the appropriate Debian packages manually."
    log ""
    log "Most are supplied by:"
    log "  coreutils"
    log "  grep"
    log "  sed"
    exit 1
fi

log "[OK] Base shell utilities."

# ============================================================
# Architecture
# ============================================================

ARCH="$(uname -m)"

log ""
log "Detected architecture: $ARCH"

case "$ARCH" in

    x86_64|amd64)
        log "[OK] Architecture supported."
        ;;

    *)
        die "Unsupported architecture: $ARCH

UGREEN currently publishes ugcli for Linux x86_64 only."
        ;;

esac

# ============================================================
# Cleanup stale state
# ============================================================

log ""
log "Checking for stale state from previous installer runs..."

cleanup_legacy_installer

# New installer always uses a unique work directory.
make_workdir

trap cleanup_workdir EXIT HUP INT TERM

log "[OK] Clean working directory:"
log "  $WORKDIR"

# ============================================================
# Determine missing SYSTEM dependencies
#
# IMPORTANT:
# No APT command is run unless something here is missing.
# ============================================================

log ""
log "========================================"
log " Checking system dependencies"
log "========================================"
log ""

MISSING_SYS=""

# ------------------------------------------------------------
# Python
# ------------------------------------------------------------

if has_cmd python3; then

    log "[OK] Python 3:"
    log "  $(python3 --version 2>&1)"

else

    log "[MISSING] python3"
    MISSING_SYS="$MISSING_SYS python3"

fi

# ------------------------------------------------------------
# CA certificates
# ------------------------------------------------------------

if pkg_installed ca-certificates ||
   [ -r /etc/ssl/certs/ca-certificates.crt ]
then

    log "[OK] CA certificates"

else

    log "[MISSING] ca-certificates"
    MISSING_SYS="$MISSING_SYS ca-certificates"

fi

# ============================================================
# Install ONLY missing system dependencies
# ============================================================

if [ -n "$MISSING_SYS" ]; then

    install_system_packages "$MISSING_SYS"

else

    log ""
    log "[OK] No system packages need installation."
    log "APT will not be touched."

fi

# ============================================================
# Reverify Python
# ============================================================

if ! has_cmd python3; then
    die "python3 is still unavailable after dependency processing."
fi

verify_python_https

# ============================================================
# pip
#
# pip is part of the requested installed environment.
#
# We avoid Debian python3-pip when possible because pip can
# safely bootstrap into the current user's environment without
# modifying UGOS's Python packages.
# ============================================================

log ""
log "========================================"
log " Checking pip"
log "========================================"
log ""

if python3 -m pip --version >/dev/null 2>&1
then

    log "[OK] pip already installed:"
    log "  $(python3 -m pip --version)"

else

    log "[MISSING] pip"
    log ""
    log "Trying Python ensurepip..."

    if python3 \
        -m ensurepip \
        --user \
        --upgrade \
        >/dev/null 2>&1 &&
       python3 -m pip --version >/dev/null 2>&1
    then

        log "[OK] pip installed using ensurepip."

    else

        log "ensurepip is unavailable."
        log ""
        log "Using official PyPA get-pip.py."

        GET_PIP="$WORKDIR/get-pip.py"

        download_file \
            "$GET_PIP_URL" \
            "$GET_PIP"

        log ""
        log "Installing pip for the current user..."

        if ! python3 \
            "$GET_PIP" \
            --user \
            --break-system-packages
        then

            log ""
            log "========================================"
            log " MANUAL ACTION REQUIRED"
            log "========================================"
            log ""
            log "Automatic pip bootstrap failed."
            log ""
            log "Install pip manually for:"
            log "  $(python3 --version 2>&1)"
            log ""
            log "Then verify:"
            log ""
            log "  python3 -m pip --version"
            exit 1
        fi

    fi

fi

if ! python3 -m pip --version >/dev/null 2>&1
then
    die "pip installation completed but could not be verified."
fi

log ""
log "[OK] pip:"
log "  $(python3 -m pip --version)"

# ============================================================
# ugcli
# ============================================================

log ""
log "========================================"
log " Checking ugcli"
log "========================================"
log ""

# Decide WHICH version we are installing before comparing anything
# against what is already on the box. Everything below this point
# treats $UGCLI_VERSION as a concrete, resolved version.
resolve_ugcli_version

UGCLI_URL="$(ugcli_url "$UGCLI_VERSION")"

log ""

INSTALL_UGCLI=1

# ------------------------------------------------------------
# Check PATH first
# ------------------------------------------------------------

if has_cmd ugcli; then

    CURRENT_PATH="$(command -v ugcli)"

    CURRENT_OUTPUT="$(
        ugcli --version 2>/dev/null ||
        true
    )"

    log "[FOUND] ugcli:"
    log "  $CURRENT_PATH"

    if [ -n "$CURRENT_OUTPUT" ]; then
        log "  $CURRENT_OUTPUT"
    fi

    if printf '%s' "$CURRENT_OUTPUT" |
       grep -Fq "$UGCLI_VERSION"
    then

        log ""
        log "[OK] ugcli $UGCLI_VERSION already installed."
        log "Skipping download."

        INSTALL_UGCLI=0

    else

        log ""
        log "[UPDATE] Installed ugcli does not match:"
        log "  requested version: $UGCLI_VERSION"

    fi

# ------------------------------------------------------------
# Also check canonical install location
# ------------------------------------------------------------

elif [ -x "$UGCLI_INSTALL" ]; then

    CURRENT_OUTPUT="$(
        "$UGCLI_INSTALL" --version 2>/dev/null ||
        true
    )"

    log "[FOUND] ugcli:"
    log "  $UGCLI_INSTALL"

    if [ -n "$CURRENT_OUTPUT" ]; then
        log "  $CURRENT_OUTPUT"
    fi

    if printf '%s' "$CURRENT_OUTPUT" |
       grep -Fq "$UGCLI_VERSION"
    then

        log ""
        log "[OK] ugcli $UGCLI_VERSION already installed."
        log "Skipping download."

        INSTALL_UGCLI=0

    fi

else

    log "[MISSING] ugcli"

fi

# ============================================================
# Download/install ugcli
# ============================================================

if [ "$INSTALL_UGCLI" -eq 1 ]; then

    UGCLI_TMP="$WORKDIR/ugcli-linux-amd64"

    log ""
    log "Downloading ugcli $UGCLI_VERSION..."

    download_file \
        "$UGCLI_URL" \
        "$UGCLI_TMP"

    # --------------------------------------------------------
    # Verify we actually downloaded a Linux ELF binary.
    #
    # Prevents accidentally installing an HTML error page.
    # --------------------------------------------------------

    log ""
    log "Verifying downloaded executable..."

    if ! python3 - "$UGCLI_TMP" <<'PY'
import sys

path = sys.argv[1]

with open(path, "rb") as f:
    magic = f.read(4)

if magic != b"\x7fELF":
    raise SystemExit(
        "Downloaded file is not an ELF executable."
    )

print("ELF executable verified.")
PY
    then

        die "The ugcli download is not a valid Linux executable."

    fi

    # --------------------------------------------------------
    # Install
    # --------------------------------------------------------

    ensure_sudo

    log ""
    log "Installing ugcli:"
    log "  $UGCLI_INSTALL"

    $SUDO mkdir -p \
        "$(dirname "$UGCLI_INSTALL")"

    if has_cmd install; then

        $SUDO install \
            -m 0755 \
            "$UGCLI_TMP" \
            "$UGCLI_INSTALL"

    elif has_cmd cp; then

        $SUDO cp \
            "$UGCLI_TMP" \
            "$UGCLI_INSTALL"

        $SUDO chmod \
            0755 \
            "$UGCLI_INSTALL"

    else

        log ""
        log "========================================"
        log " MANUAL ACTION REQUIRED"
        log "========================================"
        log ""
        log "Neither 'install' nor 'cp' is available."
        log ""
        log "Install Debian coreutils manually and rerun."
        exit 1

    fi

fi

# ============================================================
# ~/.local/bin PATH
#
# User-level pip executables normally live here.
# ============================================================

USER_BIN="$HOME/.local/bin"

if [ -d "$USER_BIN" ]; then

    case ":$PATH:" in

        *":$USER_BIN:"*)
            ;;

        *)

            if [ -w "$HOME" ]; then

                log ""
                log "Adding ~/.local/bin to PATH..."

                if [ ! -f "$HOME/.bashrc" ] ||
                   ! grep -Fq \
                       'export PATH="$HOME/.local/bin:$PATH"' \
                       "$HOME/.bashrc"
                then

                    printf \
                        '\nexport PATH="$HOME/.local/bin:$PATH"\n' \
                        >> "$HOME/.bashrc"

                fi

                PATH="$USER_BIN:$PATH"
                export PATH

            else

                warn "Could not update PATH automatically."
                warn "Add $USER_BIN to PATH manually if needed."

            fi
            ;;

    esac

fi

# ============================================================
# Final verification
# ============================================================

log ""
log "========================================"
log " Final verification"
log "========================================"
log ""

# ------------------------------------------------------------
# Python
# ------------------------------------------------------------

log "Python:"
log "  $(python3 --version 2>&1)"

# ------------------------------------------------------------
# pip
# ------------------------------------------------------------

log ""
log "pip:"
log "  $(python3 -m pip --version 2>&1)"

# ------------------------------------------------------------
# ugcli
# ------------------------------------------------------------

log ""
log "ugcli:"

if [ ! -x "$UGCLI_INSTALL" ]; then
    die "ugcli was not found at:
$UGCLI_INSTALL"
fi

UGCLI_FINAL="$(
    "$UGCLI_INSTALL" --version 2>&1 ||
    true
)"

if [ -z "$UGCLI_FINAL" ]; then
    die "ugcli exists but failed to report its version."
fi

log "  $UGCLI_FINAL"

# ============================================================
# Complete
# ============================================================

log ""
log "========================================"
log " Installation completed successfully"
log "========================================"
log ""
log "Installed/verified:"
log "  Python 3"
log "  pip"
log "  CA certificates"
log "  ugcli $UGCLI_VERSION"
log ""
log "ugcli location:"
log "  $UGCLI_INSTALL"
log ""
log "This installer deliberately did NOT run:"
log ""
log "  apt --fix-broken install"
log "  apt upgrade"
log "  apt dist-upgrade"
log ""
