"""Shared definitions for the /api/v1 contract gates (issue #166).

Ported from `scripts/api/lib.sh`. One place names the contract, its two
generated outputs and how the generator is invoked, so `generate`,
`check_contract_drift` and `check_client_paths` cannot disagree about any of
the three. They disagreeing is the exact failure that would make a green
drift check meaningless.
"""

from __future__ import annotations

import os
import subprocess
from pathlib import Path

API_CONTRACT = Path("api/v1/openapi.json")
API_GO_BINDING = Path("core/apicontract/contract.gen.go")
API_TS_BINDING = Path("ui/shared/src/api/generated/contract.ts")


def generate(out_go: Path, out_ts: Path) -> subprocess.CompletedProcess[str]:
    """Run the generator, writing the Go and TypeScript bindings to `out_go`/`out_ts`.

    `GOWORK=off` and a bare `go run`: the generator is a single stdlib-only
    program that deliberately belongs to no module, exactly like
    `scripts/architecture/ownership.go` next to it, so it runs identically in
    a fresh worktree with nothing installed.
    """
    env = dict(os.environ)
    env["GOWORK"] = "off"
    return subprocess.run(
        ["go", "run", "scripts/api/gen-bindings.go", ".", str(out_go), str(out_ts)],
        env=env,
        capture_output=True,
        text=True,
    )
