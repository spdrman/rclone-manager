#!/usr/bin/env python3
"""Capture the Phase 6 performance baseline (issue #165, EPIC B #81's
performance contract).

The contract names seven metrics and says to capture them BEFORE any
structural refactoring, because once code moves the pre-refactor number
is gone. This script is the one supported way to produce that record, so
a later run compares like with like:

  idle RSS, startup-to-healthy, /api/v1 read latency, configuration
  write latency, backup transfer throughput, image size, idle CPU.

Three harnesses produce them, each owned by the layer it measures:

  apps/generic/tests/perfbaseline  the five process/API metrics, against
                                   the real binary over real HTTP
  core/tests/perfbaseline          transfer throughput, through core's
                                   own transport adapter
  this script                     image size, by building the image

The result is written to docs/perf/baselines/<host-id>.json and is meant
to be committed. docs/perf/README.md defines the host, the workload and
the threshold a later run is judged against; check_baseline.py is what
judges it.

Nothing here handles a credential. The runtime harness reads the
engine's single-use enrollment token from a pipe and keeps it in memory;
no harness output carries anything but measurements, which is why the
records are safe to commit.

Ported from `scripts/perf/capture-baseline.sh` under EPIC I (#672 /
#662). Nothing fabricates a stand-in at that literal path the way
`scripts/tests/ci-local-gate.test.sh` does for `check-baseline.sh` and
`selftest.sh`, and no gate calls it -- `docs/perf/README.md` already says
this is run by hand, on a dedicated benchmark host, not by CI. So the
old path is deleted rather than kept as a shim; every caller (the two
docs pages that showed the command, and this file's own remedy line in
`check_baseline.py`) is updated to the new one in the same change.

Host identity (the `<host-id>.json` slug, and the descriptor stored
inside the record) is NOT reimplemented here. `scripts/perf/hostid.sh`
stays exactly what it is: a bash library, sourced both by the two
`scripts/perf/*.sh` shims this replaces and directly by
`apps/generic/tests/dockercli/imagesize_test.go`, whose own comment says
why -- the slug is a rule, and a second implementation of a rule is a
second thing to keep in step. This script calls the one implementation
the same way that Go test already does: `bash -c "source
scripts/perf/hostid.sh && perf::host_id"`.

Neither Go harness below runs under `-race`, and neither ever may. Every
other `go test` in this repository does since #417, so this is the one
place where the consistent-looking edit is the wrong one: the race
detector slows an instrumented binary by several times, and a baseline
captured from one would set a number no uninstrumented run could ever be
compared against.

# PORTED-CHECK HAZARD NOTE

jq, go and git are on PATH before anything else runs
  hazard in bash:   a missing tool otherwise fails deep inside the first
                    harness invocation, several `echo` lines into a run,
                    rather than in the first second.
  hazard in python: GONE as a verdict-class question, in the deliberate
                    direction the harness recommends: `harness.require_tools`
                    refuses with `cannot_run` rather than a bare `die`.
                    This script is never read by `scripts/ci-local.sh`'s
                    ledger (nothing gates on it), so the distinction has
                    no downstream reader today, but a missing toolchain
                    IS a capability gap rather than a proof that ran and
                    failed, and `harness.py`'s own vocabulary is for
                    saying so precisely once something does read it.
  held by:          `require_tools` at the top of `body()`.

a `go test` harness's own exit status becomes this script's, unguarded
  hazard in bash:   `set -e` on an unguarded `go test ...` makes the
                    harness's exit status the script's own. If that
                    status happened to be 3 -- the CLI's own meaning for
                    #551, "another process is already serving this
                    deployment", reachable if the harness under test ever
                    shells out to `backupd` -- a capture that never finished
                    would exit with the number this repository elsewhere
                    reserves for "this machine could not perform the
                    proof".
  hazard in python: GONE as that specific collision, the same way
                    `run_tests_repo_gate.py` closes it: `harness.sh`'s
                    default `check=True` raises `CommandFailed`, and
                    `finish()` translates ONLY `harness.cannot_run` to 3,
                    turning a subprocess's own 3 into 1 under "a command
                    exited 3" instead of forwarding it.
  held by:          `finish()`'s `CommandFailed`/status-3 branches, shared
                    with every other ported entry point.

`docker build`'s failure is not mistaken for a successful, empty image
  hazard in bash:   `docker build ... >/dev/null` under `set -e` --
                    dropping stdout while keeping the guard is the
                    correct shape, and losing the guard (a stray `|| true`)
                    is the failure mode this line has to avoid.
  hazard in python: STILL EXISTS as the same shape: `harness.sh(check=True,
                    capture=True)` discards stdout on success and raises
                    with it quoted on failure.
  held by:          the single `harness.sh` call around `docker build`.

the image is measured for the DAEMON's architecture, not this shell's
  hazard in bash:   a remote `DOCKER_HOST` (Colima, Lima, Docker Desktop
                    pointed elsewhere) can build for an architecture the
                    daemon has to emulate, silently, if the platform came
                    from the client instead.
  hazard in python: STILL EXISTS, translated line for line: the daemon
                    probe is tried first and its architecture wins;
                    `go env GOARCH` is only the fallback when there is no
                    daemon to ask, preserved so `--skip-image` still needs
                    no Docker to compute a default it will not use.
  held by:          `default_image_platform`'s daemon-probe branch, and
                    `apps/generic/tests/dockercli/imagesize_test.go`,
                    which enforces the recorded architecture against the
                    daemon's own on every gate run.
"""

from __future__ import annotations

import json
import os
import statistics
import sys
import tempfile
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from bdtools import harness

PROGRAM = "capture-baseline"

USAGE = """\
usage: python3 scripts/bdtools/perf/capture_baseline.py [options]

  --repeat N        how many runtime captures to take (default 3). The
                    recorded number for each runtime metric is the median
                    of these, and the observed spread is recorded next to
                    it, because a single capture is not separable from
                    machine noise.
  --out PATH        write the record here instead of
                    docs/perf/baselines/<host-id>.json
  --skip-image      do not build the OCI image (leaves image size null;
                    a record with a null image size is incomplete and
                    check_baseline.py says so)
  --platform PLAT   image platform to build and measure
                    (default: the docker daemon's own architecture,
                    from `docker version --format {{.Server.Arch}}`,
                    falling back to linux/$(go env GOARCH) when there is
                    no daemon to ask)
"""


class Args:
    def __init__(self) -> None:
        self.repeat = 3
        self.out: Path | None = None
        self.skip_image = False
        self.platform: str | None = None


def parse_args(argv: list[str]) -> Args:
    args = Args()
    i = 0
    while i < len(argv):
        arg = argv[i]
        if arg in ("--repeat", "--out", "--platform"):
            if i + 1 >= len(argv):
                print(f"capture-baseline: {arg} needs an argument", file=sys.stderr)
                print(USAGE, file=sys.stderr, end="")
                raise SystemExit(2)
            value = argv[i + 1]
            if arg == "--repeat":
                args.repeat = int(value)
            elif arg == "--out":
                args.out = Path(value)
            else:
                args.platform = value
            i += 2
        elif arg == "--skip-image":
            args.skip_image = True
            i += 1
        elif arg in ("-h", "--help"):
            print(USAGE, file=sys.stderr, end="")
            raise SystemExit(0)
        else:
            print(f"capture-baseline: unknown option {arg}", file=sys.stderr)
            print(USAGE, file=sys.stderr, end="")
            raise SystemExit(2)
    return args


def sh_out_at(argv: list[str], cwd: Path, *, check: bool = True) -> str:
    """`harness.sh_out`, at a working directory: that function takes none,
    and every caller here needs one (it runs after this script's own `cd`
    to the repository root, from `apps/generic` or `core` in one case)."""
    return harness.sh(argv, check=check, capture=True, cwd=cwd).stdout.rstrip("\n")


def host_id_and_json(root: Path) -> tuple[str, dict[str, Any]]:
    """The one bash implementation of the host-id rule, called the same
    way `apps/generic/tests/dockercli/imagesize_test.go` already calls it."""
    host_id = sh_out_at(["bash", "-c", "source scripts/perf/hostid.sh && perf::host_id"], root)
    host_json_text = sh_out_at(["bash", "-c", "source scripts/perf/hostid.sh && perf::host_json"], root)
    return host_id, json.loads(host_json_text)


def default_image_platform(root: Path) -> str:
    """The docker daemon's own architecture, falling back to the client's.

    Best-effort and never fatal: `--skip-image` must not need Docker to
    compute a default it will not use, so a daemon that cannot be reached
    falls back silently rather than refusing.
    """
    proc = harness.sh(
        ["docker", "version", "--format", "{{.Server.Arch}}"],
        check=False,
        capture=True,
        timeout=10,
    )
    daemon_arch = proc.stdout.strip() if proc.returncode == 0 else ""
    if daemon_arch:
        return f"linux/{daemon_arch}"
    go_arch = sh_out_at(["go", "env", "GOARCH"], root)
    return f"linux/{go_arch}"


def r3(value: float | None) -> float | None:
    if value is None:
        return None
    return round(value * 1000) / 1000


def stat_block(values: list[float]) -> dict[str, Any]:
    """Median, min, max and spread, the way `capture-baseline.sh`'s `jq`
    merge computed them: explicit rather than averaged, so one slow
    capture cannot move the recorded number."""
    if not values:
        return {"median": None, "min": None, "max": None, "spread_percent_of_median": None}
    med = statistics.median(values)
    lo = min(values)
    hi = max(values)
    spread = None if med == 0 else r3((hi - lo) / med * 100)
    return {"median": r3(med), "min": r3(lo), "max": r3(hi), "spread_percent_of_median": spread}


def run_go_capture(root: Path, module: Path, test_name: str, out_file: Path) -> dict[str, Any]:
    env = {**os.environ, "PERF_BASELINE": "1", "PERF_BASELINE_OUT": str(out_file), "GOWORK": "off"}
    harness.sh(
        ["go", "test", "./tests/perfbaseline/", "-run", test_name, "-count=1", "-timeout", "20m"],
        cwd=module,
        env=env,
        capture=True,
    )
    data: dict[str, Any] = json.loads(out_file.read_text())
    return data


def build_record(
    host_id: str,
    host_json: dict[str, Any],
    commit: str,
    dirty: bool,
    repeat: int,
    runtime_records: list[dict[str, Any]],
    transfer_record: dict[str, Any],
    image_size: int | None,
    image_arch: str | None,
    image_platform: str,
) -> dict[str, Any]:
    first = runtime_records[0]

    def runtime_values(picker: Any) -> list[Any]:
        return [picker(r) for r in runtime_records]

    return {
        "schema": "backupd/perf-baseline/1",
        "workload": first["workload"],
        "host_id": host_id,
        "host": host_json,
        "commit": commit,
        "working_tree_dirty": dirty,
        "captured_at": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "runtime_captures": repeat,
        "backup_sets": first["backup_sets"],
        "metrics": {
            "startup_to_healthy_ms": stat_block(runtime_values(lambda r: r["startup_to_healthy_ms"])),
            "idle_rss_bytes": stat_block(runtime_values(lambda r: r["idle_rss_bytes"])),
            "idle_cpu_percent": stat_block(runtime_values(lambda r: r["idle_cpu_percent"])),
            "idle_cpu_seconds_total": stat_block(runtime_values(lambda r: r["idle_cpu_seconds_total"])),
            "api_read_p95_ms": stat_block(runtime_values(lambda r: r["api_read_latency_ms"]["p95_ms"])),
            "api_read_p50_ms": stat_block(runtime_values(lambda r: r["api_read_latency_ms"]["p50_ms"])),
            "config_write_p95_ms": stat_block(runtime_values(lambda r: r["config_write_latency_ms"]["p95_ms"])),
            "config_write_p50_ms": stat_block(runtime_values(lambda r: r["config_write_latency_ms"]["p50_ms"])),
            "transfer_mb_per_second": {
                "median": transfer_record["median_mb_per_second"],
                "min": transfer_record["slowest_mb_per_second"],
                "max": transfer_record["fastest_mb_per_second"],
                "spread_percent_of_median": transfer_record["spread_percent_of_median"],
            },
            "image_size_bytes": {"value": image_size, "architecture": image_arch, "platform": image_platform},
        },
        "detail": {
            "idle_cpu_floor_percent": first["idle_cpu_floor_percent"],
            "idle_cpu_window_seconds": first["idle_cpu_window_seconds"],
            "api_read_endpoint": first["api_read_latency_ms"]["endpoint"],
            "api_read_response_bytes": first["api_read_latency_ms"]["response_bytes"],
            "api_read_samples_per_capture": first["api_read_latency_ms"]["samples"],
            "config_write_endpoint": first["config_write_latency_ms"]["endpoint"],
            "config_write_samples_per_capture": first["config_write_latency_ms"]["samples"],
            "transfer_backend": transfer_record["backend"],
            "transfer_artifact_bytes": transfer_record["artifact_bytes"],
            "transfer_repetitions": transfer_record["repetitions"],
        },
        "runtime_raw": runtime_records,
    }


def body(args: Args, root: Path) -> int:
    harness.require_tools("jq", "go", "git")

    image_platform = args.platform if args.platform is not None else default_image_platform(root)

    host_id, host_json = host_id_and_json(root)
    out_path = args.out if args.out is not None else root / "docs" / "perf" / "baselines" / f"{host_id}.json"

    commit = sh_out_at(["git", "rev-parse", "HEAD"], root)
    status = harness.sh(["git", "status", "--porcelain"], check=False, capture=True, cwd=root)
    dirty = bool(status.stdout.strip())
    if dirty:
        # Not fatal: the baseline is often captured from a working tree
        # that already holds the harness itself, which by definition is
        # not yet committed. Recorded honestly so a reader knows.
        print(
            f"capture-baseline: NOTE the working tree is dirty; recording dirty=true against {commit}",
            file=sys.stderr,
        )

    with tempfile.TemporaryDirectory(prefix="backupd-perf-") as tmp_str:
        tmp = Path(tmp_str)

        harness.step(f"runtime harness (apps/generic/tests/perfbaseline), {args.repeat} capture(s)")
        runtime_records = []
        for i in range(1, args.repeat + 1):
            harness.note(f"capture {i}/{args.repeat}")
            runtime_records.append(
                run_go_capture(root, root / "apps" / "generic", "TestCaptureRuntimeBaseline", tmp / f"runtime-{i}.json")
            )

        harness.step("transfer harness (core/tests/perfbaseline)")
        transfer_record = run_go_capture(root, root / "core", "TestCaptureTransferBaseline", tmp / "transfer.json")

        image_size: int | None = None
        image_arch: str | None = None
        if not args.skip_image:
            harness.step(f"image size (docker build --platform {image_platform} -f container/Dockerfile)")
            harness.require_docker()
            tag = f"backupd-perfbaseline:{commit[:12]}"
            harness.sh(
                ["docker", "build", "--platform", image_platform, "-f", "container/Dockerfile", "-t", tag, "."],
                cwd=root,
                capture=True,
            )
            image_size = int(sh_out_at(["docker", "image", "inspect", tag, "--format", "{{.Size}}"], root))
            image_arch = sh_out_at(["docker", "image", "inspect", tag, "--format", "{{.Architecture}}"], root)

        harness.step(f"merging into {out_path}")
        out_path.parent.mkdir(parents=True, exist_ok=True)
        record = build_record(
            host_id,
            host_json,
            commit,
            dirty,
            args.repeat,
            runtime_records,
            transfer_record,
            image_size,
            image_arch,
            image_platform,
        )
        out_path.write_text(json.dumps(record, indent=2) + "\n")

    print(f"OK: wrote {out_path}")
    print(json.dumps(record["metrics"], indent=2))
    return harness.EXIT_OK


def main(argv: list[str]) -> int:
    harness.set_program(PROGRAM)
    harness.install_signal_handlers()
    args = parse_args(argv)
    root = harness.repo_root(Path(__file__))
    return harness.finish(lambda: body(args, root))


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
