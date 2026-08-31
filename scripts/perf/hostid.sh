# Host identity for the Phase 6 performance baselines (issue #165).
#
# A baseline number means nothing without the machine it was taken on, and
# "the same host" has to be decidable by a script rather than by someone
# remembering. These two helpers produce a stable slug (used as the
# baseline record's filename) and the full descriptor stored inside it, so
# scripts/perf/check-baseline.sh can refuse to compare a run against a
# baseline captured somewhere else instead of quietly reporting a
# regression that is really a different computer.
#
# Not meant to be executed directly - sourced by the scripts next to it.

# perf::host_id prints the slug: os-arch-model, lowercased, with anything
# outside [a-z0-9._-] collapsed to a dash.
perf::host_id() {
  local os arch model
  os=$(uname -s | tr '[:upper:]' '[:lower:]')
  arch=$(uname -m)
  model=$(perf::host_model)
  printf '%s' "${os}-${arch}-${model}" |
    tr '[:upper:]' '[:lower:]' |
    sed 's/[^a-z0-9._-]\{1,\}/-/g; s/^-//; s/-$//'
}

# perf::host_model prints the machine model, however this platform names
# one, or "unknown" where nothing reports it.
perf::host_model() {
  case "$(uname -s)" in
    Darwin)
      sysctl -n hw.model 2>/dev/null || echo unknown
      ;;
    Linux)
      if [ -r /sys/devices/virtual/dmi/id/product_name ]; then
        cat /sys/devices/virtual/dmi/id/product_name
      else
        echo unknown
      fi
      ;;
    *)
      echo unknown
      ;;
  esac
}

# perf::host_json prints the full descriptor as a JSON object.
perf::host_json() {
  local os os_version arch model cpu cores memory
  os=$(uname -s)
  arch=$(uname -m)
  model=$(perf::host_model)
  case "$os" in
    Darwin)
      os_version=$(sw_vers -productVersion 2>/dev/null || uname -r)
      cpu=$(sysctl -n machdep.cpu.brand_string 2>/dev/null || echo unknown)
      cores=$(sysctl -n hw.ncpu 2>/dev/null || echo 0)
      memory=$(sysctl -n hw.memsize 2>/dev/null || echo 0)
      ;;
    Linux)
      os_version=$(uname -r)
      cpu=$(awk -F': ' '/model name/{print $2; exit}' /proc/cpuinfo 2>/dev/null || echo unknown)
      [ -n "$cpu" ] || cpu=unknown
      cores=$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 0)
      memory=$(awk '/MemTotal/{print $2 * 1024; exit}' /proc/meminfo 2>/dev/null || echo 0)
      ;;
    *)
      os_version=$(uname -r)
      cpu=unknown
      cores=0
      memory=0
      ;;
  esac

  jq -n \
    --arg os "$os" \
    --arg os_version "$os_version" \
    --arg arch "$arch" \
    --arg model "$model" \
    --arg cpu "$cpu" \
    --argjson cores "${cores:-0}" \
    --argjson memory "${memory:-0}" \
    '{os: $os, os_version: $os_version, arch: $arch, model: $model, cpu: $cpu, cores: $cores, memory_bytes: $memory}'
}
