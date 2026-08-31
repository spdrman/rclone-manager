# Sourced by every lifecycle script in this package. Not itself a
# lifecycle stage.
#
# Every path here is derived from documented ground. Synology's package
# FHS documents /var/packages/<pkg>/{target,etc,var,tmp,home} and which of
# them survive an upgrade or an uninstall; SYNOPKG_PKGNAME and
# SYNOPKG_PKGDEST are two of the environment variables the script
# environment page lists. There is deliberately no use of
# SYNOPKG_PKGVAR/SYNOPKG_PKGETC: neither appears in that documented list,
# so this package spells the FHS paths out instead of depending on a
# variable that may not be exported on a given DSM build.

# The two variables every path in this package is derived from, asserted
# before anything is derived from them. "Documented" is not "exported on
# the DSM build in front of you", and with SYNOPKG_PKGNAME unset PKG_VAR
# becomes /var/packages/var: a real directory outside this package that
# postinst would chmod 0750 and that start-stop-status would create files
# under and delete files from. Refusing to run is the only safe answer,
# and it is the precondition every other path guarantee here rests on.
: "${SYNOPKG_PKGNAME:?SYNOPKG_PKGNAME is not set - refusing to derive package paths from an empty package name}"
: "${SYNOPKG_PKGDEST:?SYNOPKG_PKGDEST is not set - refusing to derive package paths from an empty target directory}"

PKG_VAR="/var/packages/${SYNOPKG_PKGNAME}/var"
PKG_ETC="/var/packages/${SYNOPKG_PKGNAME}/etc"
PKG_BIN="${SYNOPKG_PKGDEST}/bin"

# target/ is replaced wholesale on every upgrade and removed on uninstall,
# so nothing that must survive either may live under PKG_BIN. etc/ and
# var/ persist through both, which is why the config file, the SQLite
# journal and the local-auth administrator record all live there.
CONFIG_FILE="${PKG_ETC}/config.yaml"
STATE_DIR="${PKG_VAR}/state"
LOG_DIR="${PKG_VAR}/log"
RUN_DIR="${PKG_VAR}/run"

# The shared UI bundle this package carries (issue #180). Under target/,
# which DSM replaces wholesale on upgrade: it is a build artifact of the
# release, not state. serve-ui is pointed at it explicitly rather than
# left to the bundle compiled into the binary, because that one is the
# generic bridge and this is a Synology install.
UI_BUNDLE_DIR="${SYNOPKG_PKGDEST}/ui-bundle"

# The runtime profile both processes select. It changes the platform this
# runtime reports itself as and the UI bridge it serves, and nothing about
# backup lifecycle, retention or validation.
RUNTIME_PROFILE="synology"

ENGINE_LOG="${LOG_DIR}/engine.log"
UI_LOG="${LOG_DIR}/ui.log"
ENGINE_PID="${RUN_DIR}/engine.pid"
UI_PID="${RUN_DIR}/ui.pid"

# The two listeners. UI_PORT is INFO's adminport and the only LAN-facing
# one; the engine binds loopback and is reached only through the UI host's
# own reverse proxy, exactly as container/compose.yaml's two services do.
UI_PORT=8477
ENGINE_ADDR="127.0.0.1:8478"

# The ceiling on each log file, in bytes. Both logs live under var/ on the
# DSM system volume, var/ survives every upgrade and reboot, and this
# package is meant to run for years: unrotated, the engine's per-cycle
# output fills the one volume a NAS cannot afford to fill, and a full
# system volume takes down every package on the unit. The container
# profiles inherit Docker's log driver for this; bound_logs below is the
# package's equivalent.
LOG_MAX_BYTES=8388608

# Report a message to whoever asked DSM to run this stage. Package Center
# shows SYNOPKG_TEMP_LOGFILE's contents on failure, which is the only
# channel a lifecycle script has to explain itself.
say() {
    echo "$@"
    if [ -n "${SYNOPKG_TEMP_LOGFILE}" ]; then
        echo "$@" >> "${SYNOPKG_TEMP_LOGFILE}"
    fi
}

# What is the process with this pid actually running?
#
# /proc is the answer on DSM. ps is a fallback so this function can be
# exercised on a development host with no procfs, and so an unreadable
# /proc entry does not silently become a yes. Either way an empty answer
# means "not ours", which is the safe direction for both callers.
pid_command() {
    if [ -r "/proc/$1/cmdline" ]; then
        tr '\0' ' ' < "/proc/$1/cmdline"
    else
        ps -o args= -p "$1" 2>/dev/null
    fi
}

# Is the process recorded in a pid file still OUR process?
#
# Not "does a process with this pid exist". var/ survives reboots,
# upgrades and uninstalls, so a pid file outlives the pid space that wrote
# it, and after one unclean shutdown the pid it names is whatever the
# kernel handed out next. Both callers act on the answer and both are
# destructive if it is wrong: status reports the package healthy while
# nothing is running, and stop sends SIGTERM and then SIGKILL to that pid.
# So identity is checked, not just existence.
pid_alive() {
    pidfile="$1"
    [ -s "${pidfile}" ] || return 1
    pid=$(cat "${pidfile}" 2>/dev/null)
    [ -n "${pid}" ] || return 1
    kill -0 "${pid}" 2>/dev/null || return 1
    case "$(pid_command "${pid}")" in
        *"${PKG_BIN}/backup-manager-web"*)
            return 0
            ;;
    esac
    return 1
}

# Is a log file over the ceiling?
#
# The -f test is not redundant with the redirection below: a failed input
# redirection is reported by the shell itself, on the shell's own stderr,
# where the 2>/dev/null cannot reach it. A fresh install has no log files
# at all and start calls this before either daemon has written one, so
# without the test every first start prints two errors into the install
# log that mean nothing.
log_oversize() {
    [ -f "$1" ] || return 1
    _sz=$(wc -c < "$1" 2>/dev/null || echo 0)
    [ "${_sz:-0}" -gt "${LOG_MAX_BYTES}" ]
}

# Bound both logs to one generation each.
#
# Copy-then-truncate rather than rename: the daemons hold their log open
# and append to the inode, so renaming the file would leave them filling
# the renamed one and bound nothing at all. The copy overwrites the single
# older generation, so two files is the ceiling per log. A few lines
# written between the copy and the truncation are lost, which is the
# accepted cost of bounding a log nobody can rotate with a signal.
#
# Written with literal paths rather than a parameter so that every path
# this touches resolves statically for ScanForUnsafeDeletes.
bound_logs() {
    if log_oversize "${ENGINE_LOG}"; then
        cp "${ENGINE_LOG}" "${ENGINE_LOG}.1"
        : > "${ENGINE_LOG}"
    fi
    if log_oversize "${UI_LOG}"; then
        cp "${UI_LOG}" "${UI_LOG}.1"
        : > "${UI_LOG}"
    fi
}
