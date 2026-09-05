package testtier

// LedgerEntry is one file known to be in the wrong tier, and the issue
// that moves it. The ledger is the migration, written down where the
// guard can hold it: a file that stops violating has to come off the
// ledger in the same change, and a file that is not on it cannot start.
type LedgerEntry struct {
	// File is relative to the core module root, forward slashes.
	File string
	Rule string
	// Issue is the follow-up that empties this entry.
	Issue int
}

// Ledger is empty, and #448 and #450 are what emptied it.
//
// It held eight entries in two shapes. Six were container-backed tests
// living in unit packages, which is why `go test ./internal/...` needed a
// Docker daemon and why core/internal/transport/rclone and core/service
// were the two suites that went red under concurrent gate load for reasons
// that had nothing to do with the code under test. They are
// core/tests/machinegate now, and the pure halves of the three mixed files
// stayed where they were.
//
// The other two were integration tests that exec'd docker themselves rather
// than through a harness. The harness has those two capabilities now
// (Source.Kill, to remove its own container on purpose, and
// Medium.HasBucket, to look inside MinIO's drive), so the bypasses-harness
// rule holds with no exceptions at all.
//
// Keeping the mechanism with an empty slice is deliberate. The ledger is
// how a migration is written down where the guard can hold it, and the next
// one should find the shape already here rather than reinventing it. An
// empty ledger also means the guard is now a plain rule: a new violation is
// unexpected, full stop, with nothing to add itself to.
var Ledger = []LedgerEntry{}

// Key is what a finding and a ledger entry are matched on.
type Key struct {
	File string
	Rule string
}

// Diff compares a report against a ledger. Unexpected are findings for
// which no entry exists, grouped by file and rule, with the first line the
// scan saw. Stale are ledger entries no finding backs any more, which means
// the file was fixed and the ledger was not, or the file moved.
func Diff(rep Report, ledger []LedgerEntry) (unexpected []Finding, stale []LedgerEntry) {
	known := map[Key]bool{}
	for _, e := range ledger {
		known[Key{e.File, e.Rule}] = true
	}
	seen := map[Key]bool{}
	for _, f := range rep.Findings {
		k := Key{f.File, f.Rule}
		seen[k] = true
		if !known[k] {
			// One line per file and rule is what a reader needs; the
			// scan's own ordering puts the first line first.
			dup := false
			for _, u := range unexpected {
				if u.File == f.File && u.Rule == f.Rule {
					dup = true
					break
				}
			}
			if !dup {
				unexpected = append(unexpected, f)
			}
		}
	}
	for _, e := range ledger {
		if !seen[Key{e.File, e.Rule}] {
			stale = append(stale, e)
		}
	}
	return unexpected, stale
}
