"""Issue #526's package-documentation gate: does every package's `go doc`
overview still say what it is recorded as saying, and is it still
assembled from the same files, and is a Go change proven to touch nothing
but comments where a change claims exactly that?

Three entry points:

  * `check_package_doc`   -- the gate itself. Presence mode compares every
    package's assembled overview to `scripts/docs/package-doc.baseline`;
    `--against <ref>` compares two trees' overviews to each other instead
    of either to the baseline. This is the one `scripts/ci-local.sh` runs.
  * `check_comments_only` -- is a Go change, ref to work tree, provably
    comments-and-directives-only? The independent second opinion
    `selftest`'s G3/G4 pair needs.
  * `selftest`            -- positive controls, promoting a real file
    opener in `core/service/activity.go` to sit adjacent to `package` and
    requiring `check_package_doc` to go red and name it, while requiring
    `check_comments_only` to stay silent on the same promotion.

Both checks call `core/cmd/docguard` (unchanged Go, the reader for both
facts) as a subprocess exactly the way the bash they replace did; nothing
here reimplements go/doc's own reading of a package overview or a
go/scanner token stream.

Ported from `scripts/docs/*.sh` under EPIC I (#672 / #662).
`scripts/docs/check-package-doc.sh` and `scripts/docs/selftest.sh` stay as
real, runnable shims: `scripts/tests/ci-local-gate.test.sh` fabricates a
stand-in at both literal paths. `scripts/docs/check-comments-only.sh` is
deleted outright -- nothing fabricates a stand-in there and its only
caller was `scripts/docs/selftest.sh`, which now calls
`check_comments_only.body` directly.
"""
