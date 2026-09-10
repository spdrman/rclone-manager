"""The end-to-end drivers: the things that stand a product up and drive it.

Four entry points, each runnable on its own and each speaking
`rcmtools.harness`'s vocabulary rather than its own:

  * `run_tests_repo_gate` -- the per-commit browser and CLI signal from
    the pinned `rclone-manager-tests` checkout (#158, #197);
  * `run_machine_tier`    -- the Go machine tier, run from inside a
    manager machine (#451);
  * `bump_tests_pin`      -- moves `scripts/e2e/tests-repo.pin`;
  * `two_machine_backup`  -- two throwaway machines and one real backup
    (#356).

`tests_pin` is the one thing in here that is not an entry point: the pin
file used to be read by `. scripts/e2e/tests-repo.pin` from two different
scripts, so the parser that replaces that sourcing has two real callers
and lives beside them rather than in the harness, which has no business
knowing what a pin is.

Ported from `scripts/e2e/*.sh` under EPIC I (#672 / #662). Standard
library only, Python 3.8 or newer, for the reason
`scripts/rcmtools/__init__.py` gives.
"""
