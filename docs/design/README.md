# What is in this directory

Design artifacts, kept because the decisions in them are still the ones the
product ships. Nothing here is generated and nothing here is a build input.

`Backupd.dc.html` is the interactive design the shared frontend was built
from: every screen, both themes, every provider treatment and the risk states.
`docs/deployment.md` cites it as the source for what the UI is supposed to do
when the two disagree.

`Logo Options.dc.html` is the logo exploration. **Option 1a, "Cycle", is the one
that was selected**, and it ships as `ui/shared/public/icon.svg`, tinted by
`--accent` so no provider carries an asset of its own. That sentence is the only
record of which option won, so it lives here rather than in a pull request
somebody has to go and find.

The `*.html` files with a matching `*.png` are per-issue design notes: one screen
or one interaction, the reasoning next to the picture, dated by the issue that
produced them. They are a record of a decision at the moment it was taken, so
they are not kept in step with later renames or later UI work. Read them for why
something is shaped the way it is, never as a statement about what the current
build does.

`tier-destinations.md` is the same thing in prose for the storage-tier work.

## What used to be here

`PR_BODY.md` was the pull request description for, committed to the tree by
accident along with the frontend it described. Every product decision in it is
recorded properly somewhere else: the dependency and provider-isolation
invariants in `docs/EPIC-B-multi-nas.md` §7.1, service health versus backup
health and the "Backups", not "Restore points", naming in the same document, the
colour-alone and version-mismatch rules likewise, and the "no DSM native auth is
claimed" position in `apps/synology/README.md`. Its own provider matrix had gone
stale, listing seven adapters when `apps/` carries eleven, and three of the files
it pointed at are gone (`ui/shared/src/test/providerConformance.test.tsx`,
`ui/shared/src/test/bridges.ts` and `ui/shared/e2e/README.md`). So it went, and
the one thing it alone recorded, the logo option, is two paragraphs up.
