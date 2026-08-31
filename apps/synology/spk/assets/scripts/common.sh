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

ENGINE_LOG="${LOG_DIR}/engine.log"
UI_LOG="${LOG_DIR}/ui.log"
ENGINE_PID="${RUN_DIR}/engine.pid"
UI_PID="${RUN_DIR}/ui.pid"

# The two listeners. UI_PORT is INFO's adminport and the only LAN-facing
# one; the engine binds loopback and is reached only through the UI host's
# own reverse proxy, exactly as container/compose.yaml's two services do.
UI_PORT=8477
ENGINE_ADDR="127.0.0.1:8478"

# Report a message to whoever asked DSM to run this stage. Package Center
# shows SYNOPKG_TEMP_LOGFILE's contents on failure, which is the only
# channel a lifecycle script has to explain itself.
say() {
    echo "$@"
    if [ -n "${SYNOPKG_TEMP_LOGFILE}" ]; then
        echo "$@" >> "${SYNOPKG_TEMP_LOGFILE}"
    fi
}

# Is the process recorded in a pid file still alive?
pid_alive() {
    pidfile="$1"
    [ -s "${pidfile}" ] || return 1
    pid=$(cat "${pidfile}" 2>/dev/null)
    [ -n "${pid}" ] || return 1
    kill -0 "${pid}" 2>/dev/null
}
