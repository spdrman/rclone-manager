# Contributing to rclone-manager

## Your first pull request needs one extra file

Add `contributors/<your-github-username>.md` with the filled-in signing block
from [CONTRIBUTOR-LICENSE-AGREEMENT.md](CONTRIBUTOR-LICENSE-AGREEMENT.md),
committed by you.

That is the whole of it, and it is once per contributor rather than once per
pull request. You keep the copyright in what you write; the agreement licenses
it in under Apache-2.0 so it can ship with everything else.

If you are contributing as part of your job, your employer may own the
copyright, in which case a corporate agreement is the right instrument rather
than the individual one. Say so in the pull request and one will be produced.

## What a pull request needs

**It must close an issue.** Put `Closes #N` in the body. If no issue describes
what you are doing, open one first: the issue is where the problem is argued and
the pull request is where the solution is, and separating them is what lets
somebody disagree with the fix without relitigating the problem.

**Tests come first, and they must fail before they pass.** A test that has
never been red is a test nobody has checked. Where practical, push the failing
test, then the fix, so the history shows the test catching the thing.

**Run the gate.** `scripts/ci-local.sh` is the same gate CI runs and it takes
about 45 minutes, so start it and go and do something else. It is also the
pre-commit hook, which is why committing with `--no-verify` and running the
gate deliberately is the normal working pattern rather than a shortcut.

## Things this project cares about more than most

**Say what is not proven.** The README and the site both carry a section
listing what has not been demonstrated. Adding a capability means saying
honestly what has and has not been tested about it, including on hardware
nobody here owns.

**A comment that is wrong is worse than no comment.** Several defects in this
repository's history were introduced by somebody trusting a comment that had
quietly stopped being true. If you change behaviour, read the comments around
it and fix the ones your change falsified.

**Third-party material has to be declarable.** The licence inventory and NOTICE
are generated from the module graph and the frontend lockfile, and they claim to
account for everything. Vendoring source that no package manager can see means
declaring it, or those documents become false.

**Never write a credential to disk or into a log.** The engine holds SSH private
keys, object-storage credentials and a deployment key. Several mechanisms exist
specifically so that a secret cannot reach a log line; do not route around them.
