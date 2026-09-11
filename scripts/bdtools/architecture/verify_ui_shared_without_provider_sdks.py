#!/usr/bin/env python3
"""Does `ui/shared` install and build with every provider SDK directory
removed? (EPIC-B WP1.1 RED plan, docs/EPIC-B-multi-nas.md §69 WP1.1, §11,
extended by issue #165 to Phase 6's full platform list and to a static
import scan.)

Two halves, and they catch different things.

 1. DELETION. Every provider directory is removed in a throwaway worktree
    and `ui/shared` is installed and built there. This catches anything
    that resolves a provider path at build time, including the
    "@platform-entry" alias `ui/shared/vite.config.ts` resolves into
    `apps/<platform>/frontend/`.

    `apps/generic/` is the vendor-neutral baseline the default Vite build
    targets (`VITE_PLATFORM` defaults to "generic") and `apps/common/`
    carries no frontend of its own, so neither is a "provider SDK
    directory" in the sense this check cares about and both stay. Every
    actual NAS-vendor directory goes.

 2. IMPORT SCAN, in `check-ui-shared-provider-imports.sh`, run first from
    here so a violation reports in seconds rather than after an `npm ci`.
    It covers Portainer, CasaOS, ZimaOS and Dockge, which have no
    directory to delete yet, and it is what makes #165's acceptance
    criterion true over the full Phase 6 platform list rather than only
    over the six shipped ones. See that script for why it matches module
    specifiers rather than whole lines.

The scan is invoked as `bash scripts/architecture/check-ui-shared-provider-imports.sh`,
the same command the bash used, and deliberately not as an import of that
check's module: the `.sh` path is the one `selftest.sh` drives and the one
CI names, so running it here keeps this half of the proof pointed at the
same artifact everything else in the repository uses.

Ported from `scripts/architecture/verify-ui-shared-without-provider-sdks.sh`
under EPIC I (I1.6 / #672 / #697). That path stays as a real, runnable
`exec` shim: `scripts/ci-local.sh` and `.github/workflows/ci.yml` both name
it, and `scripts/tests/ci-local-gate.test.sh` FABRICATES a file at that
literal path in its synthetic-tree fixture.

# PORTED-CHECK HAZARD NOTE

  the import scan's verdict is this check's verdict too
    hazard in bash:   none; `set -euo pipefail` made
                       `bash scripts/architecture/check-ui-shared-provider-imports.sh`
                       exiting non-zero end the run before the `npm ci`.
    hazard in python: LIVE, and it is the quietest failure in this file:
                       a `subprocess.run` on the scan whose status nobody
                       reads would let a real provider-SDK import in
                       `ui/shared` print its own FAIL text, then let this
                       script go on to build a tree where those
                       directories are deleted -- which succeeds, because
                       the import scan's whole point is to cover the four
                       platforms that have no directory to delete. Green
                       overall, with the violation printed halfway up the
                       log.
    held by:          `harness.sh`'s default `check=True` on the scan
                       call, and `harness.finish` returning the scan's
                       own status. Mutation control: a provider-SDK
                       import planted in `ui/shared/src` turns this red
                       at the scan step.

  the npm install and the Vite build decide the verdict
    hazard in bash:   none; `set -euo pipefail` again, over
                       `(cd "$wt/ui/shared" && npm ci ...)` and
                       `(cd "$wt/ui/shared" && npm run build)`.
    hazard in python: LIVE; defect #1 of this programme. `npm run build`
                       is `tsc -b && vite build`, so a `ui/shared` file
                       importing a deleted `apps/<provider>/frontend`
                       path fails there and nowhere else -- that failure
                       IS the proof. Discarding the status prints
                       "OK: ui/shared builds with every provider SDK
                       directory removed." over a build that did not.
    held by:          `harness.sh`'s default `check=True` on both npm
                       calls. Mutation control: a `ui/shared` module
                       importing `apps/ugos/frontend/platform` builds
                       clean on the real tree and fails inside the
                       worktree.

  the deletion loop cannot be aimed at the wrong root
    hazard in bash:   `rm -rf "${wt:?}/apps/${provider}"`. The `:?`
                       guarded the variable a typo controls: an unset or
                       empty `wt` would have made the operand
                       `/apps/ugos`, an absolute path outside the
                       worktree.
    hazard in python: GONE, and replaced rather than merely dropped:
                       `tree` is the `Path` yielded by
                       `arch.worktree()`, which is a fresh `mkdtemp`
                       result. There is no name here that can be empty
                       or unset -- an unbound name is a `NameError`, not
                       an empty string spliced into a path -- so the
                       shape of failure `:?` existed to catch cannot
                       occur. `tree / "apps" / provider` also cannot
                       become absolute the way string concatenation can.
    held by:          `arch.worktree`'s contract (it yields a real
                       temporary directory or raises), and the `Path`
                       join rather than an f-string.

  every provider directory named is actually removed
    hazard in bash:   NOT GUARDED, deliberately, and this port keeps the
                       gap rather than closing it. `PROVIDERS` lists the
                       full Phase 6 platform list including the four
                       with no directory today (portainer, casaos,
                       zimaos, dockge); `rm -rf` on a missing path is a
                       no-op and the bash's own comment says so. The
                       consequence is real: if every provider directory
                       vanished from HEAD, the deletion half would
                       delete nothing and the build would pass. What
                       covers that today is half 2, the import scan,
                       which is exactly why the two halves are in one
                       script.
    hazard in python: STILL EXISTS, identically and on purpose. Adding a
                       "did it exist" guard here during a port would
                       make this check disagree with its own comment and
                       with `check-ui-shared-provider-imports.sh` about
                       which platforms are expected to have a directory.
    held by:          nothing in this file, stated so the gap is on the
                       record. `verify_core_without_distribution` is the
                       check in this domain that DOES guard its delete
                       count, because its list comes out of an editable
                       text file rather than from source.

  the throwaway worktree is removed even when the build dies mid-way
    hazard in bash:   `trap cleanup EXIT`, deliberately a trap rather
                       than a line at the end, because `npm ci` and
                       `npm run build` run inside a tree full of holes:
                       an interrupt or a failing build would otherwise
                       leave a REGISTERED git worktree behind, and it
                       would contain a `node_modules` this run created.
    hazard in python: GONE as a hand-written concern, replaced by
                       `arch.worktree`'s `contextmanager`: its `finally`
                       runs `git worktree remove --force` on any exit
                       path, including an exception from a failed build.
                       `harness.install_signal_handlers` covers INT and
                       TERM by turning both into an exception, which is
                       strictly more than bash's EXIT trap gave on
                       SIGTERM -- and this is the check where it matters
                       most, since an `npm ci` is the longest window an
                       operator is likely to interrupt.
    held by:          `arch.worktree`'s `finally` clause, plus
                       `harness.install_signal_handlers()` in `main`.
"""

from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from bdtools import architecture as arch
from bdtools import harness

PROGRAM = "verify-ui-shared-without-provider-sdks"

# The full Phase 6 platform list. The four with no directory today are
# deliberately listed: rm -rf on a missing path is a no-op, and the import
# scan below is what actually covers them until #170 creates them.
PROVIDERS = (
    "ugos",
    "synology",
    "truenas",
    "unraid",
    "openmediavault",
    "proxmox",
    "portainer",
    "casaos",
    "zimaos",
    "dockge",
)

IMPORT_SCAN = "scripts/architecture/check-ui-shared-provider-imports.sh"


def body(root: Path) -> int:
    print("==> provider-SDK import scan (check-ui-shared-provider-imports.sh)")
    harness.sh(["bash", IMPORT_SCAN], cwd=root, capture=False)

    # ---- 1. deletion proof ------------------------------------------------

    with arch.worktree(root) as tree:
        for provider in PROVIDERS:
            harness.sh(["rm", "-rf", str(tree / "apps" / provider)])

        shared = tree / "ui" / "shared"

        print("==> npm ci (ui/shared, with every provider SDK directory removed)")
        harness.sh(["npm", "ci", "--no-audit", "--no-fund"], cwd=shared, capture=False)

        print("==> npm run build (ui/shared, with every provider SDK directory removed)")
        harness.sh(["npm", "run", "build"], cwd=shared, capture=False)

    print("OK: ui/shared builds with every provider SDK directory removed.")
    return harness.EXIT_OK


def main() -> int:
    harness.set_program(PROGRAM)
    harness.install_signal_handlers()
    root = arch.resolve_toplevel()
    return harness.finish(lambda: body(root))


if __name__ == "__main__":
    sys.exit(main())
