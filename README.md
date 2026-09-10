<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/logo-dark.svg">
    <img src="docs/assets/logo-light.svg" alt="rclone-manager mark: a broken ring standing for a transfer cycle in progress, next to the rclone-manager wordmark" width="240">
  </picture>
</p>


A backup lifecycle manager for a NAS. It pulls completed backup artifacts off a remote
server over SFTP, verifies them, commits them durably, and only then deletes the remote
copy.

It is a standalone Go binary that **embeds pinned rclone Go packages**. It does not fork
rclone, and it does not shell out to the `rclone` CLI for normal data movement. There are
two surfaces over the same engine: `rbm` at a terminal, and a web UI on the LAN. Everything
an operator can DO in the browser has an equivalent command, and that is a gate rather than
an intention: a route with neither a command behind it nor a written reason there is none
fails the build.

**If you just want to run it:** [Installing it](#installing-it) is two containers and a
Compose file, or one command from
[`scripts/install/install_docker_host.py`](scripts/install/install_docker_host.py) on a
machine you have SSH on.

**If you're here because a backup didn't arrive and it's 3am:** skip to
[Recovery](#recovery-when-a-backup-did-not-arrive) below, or go straight to
[`docs/recovery.md`](docs/recovery.md).

## The rule everything else serves

> A remote backup artifact MUST NOT be deleted until a verified and durably committed NAS
> copy exists. If state is uncertain, preserve the remote copy.

Since EPIC E that rule has a second half, because a durable copy no longer has to be a file
on the NAS. FR-30 in plain words: **at no instant may a completed backup have no confirmed
readable copy.** Not "usually", not "except during a move". A relocation from local disk to
a bucket copies first, proves what arrived, records that proof durably, and only then
removes what it came from, and the ordering is a table of legal transitions rather than a
convention somebody could write a function around. Where the answer is genuinely unknown,
the copy stays.

Three consequences worth having in your head before reading anything else here.

**It refuses rather than guesses.** A remote delete whose identity check comes back merely
plausible is declined, and against the hardened SFTP account this project's own setup guide
recommends that is the normal outcome rather than an edge case. A retention apply whose
inventory moved since the plan was printed is declined with nothing deleted. A
configuration write that cannot reach the process actually serving this deployment is
declined with the file left byte for byte as it was. Every one of those is a refusal you
can read, with an exit code you can branch on.

**It will not lean on a connection nobody has proved.** There are two of those, the SSH
connection to a source and the connection to a storage destination, and until 0.3.3 only
one of them was checked. Both are now proved before the product relies on them, both refuse
on failure, both spell the escape hatch `--no-verify`, and both mark what was written under
it as unverified until a passing check clears the mark. See
[Proving a connection before anything depends on it](#proving-a-connection-before-anything-depends-on-it).

**It says what it is doing while it does it.** Every operator-visible action reports a
start and a completion carrying a real outcome, on one feed that the terminal docked to the
browser window, each backup set's own page and `rbm activity --follow` are three readings
of. See [What the browser actually gives you](#what-the-browser-actually-gives-you).

Every section below is either explaining how those rules are enforced or admitting where the
enforcement doesn't exist yet.



## Licence

Apache License 2.0. The full text is in [`LICENSE`](LICENSE).

Every third-party component that reaches a shipped artifact is permissive
(MIT, BSD-2-Clause, BSD-3-Clause, Apache-2.0, CC0-1.0) except two:
`go-cleanhttp` and `go-retryablehttp` are MPL-2.0 and arrive under
rclone's `s3` backend, which cannot be registered without them. MPL-2.0 is
file-level weak copyleft and §3.3 permits this Larger Work to ship under
Apache-2.0, so the choice stands, and the §3.2 obligation it carries is
recorded in `compliance.json` and discharged in `NOTICE` and
[`docs/compliance/source-offer.md`](docs/compliance/source-offer.md), which
name both modules at their exact versions and give the immutable address their
source is served from.
