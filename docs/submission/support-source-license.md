# Support, source and licence materials

The three things every one of the targeted stores asks a submission to name, in the form
they ask for them.

## Support

- Support and bug reports: https://github.com/spdrman/rclone-manager/issues
- Response expectation: this is a single-maintainer project. Issues are read; there is no
  service level attached to them and the listing does not imply one.
- Before opening an issue, the recovery documentation covers the three failures that
  account for most of them: a backup that stopped arriving, a retention apply that
  refused, and a changed SSH host key. It is written for an administrator with no terminal
  access. See `docs/recovery-without-a-terminal.md`, and `docs/recovery.md` for the
  version that assumes one.

## Source

- Source repository: https://github.com/spdrman/rclone-manager
- The application is built from that repository. Every provider package in it wraps one
  canonical container image built from one source tree; no target rebuilds the application
  from a different set of inputs.
- Build provenance is recorded in `container/release-manifest.json`, which pins the commit
  and the SHA-256 of each core binary per architecture. See the caveat below.

## Licence

Not yet stated, and deliberately left blank here rather than filled in with a guess.

Choosing the licence a project is published under is the project owner's decision, and
B5.2 (#88) is the work package that makes it, along with the software bill of materials
and the third-party licence inventory that go with it. This preflight records the licence
row as blocked on #88 rather than inventing an answer, and the submission gate reports it
as undecided rather than letting a green run imply a licence nobody chose.

Every store on this project's list requires a licence before it will accept a submission,
so this is a real blocker on submitting rather than a paperwork item. It is a small one:
it needs a decision, not an implementation.

## A note on build provenance

`container/release-manifest.json` currently pins a commit that is not an ancestor of the
main branch, so the hashes it records describe a build that is not in this history. Until
#174 repins it, no claim that the bytes a store receives are the bytes this repository
built can actually be checked. The preflight reports that as undecided for every target
rather than passing it, and this paragraph is here so a reviewer reading the submission
materials is told the same thing the gate is.
