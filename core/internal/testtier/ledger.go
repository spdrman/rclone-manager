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

// Ledger is every file the scan finds today. Six, all one shape:
// container-backed tests living in unit packages, which is why
// `go test ./internal/...` needs a Docker daemon and why those two packages
// were the ones that went red under concurrent gate load. #448 moves them
// into the machine tier.
//
// The two bypasses-harness entries that used to be here are gone. #450 gave
// the harness the two capabilities they were exec'ing docker for
// (Source.Kill, to remove its own container on purpose, and Medium.HasBucket,
// to look inside MinIO's drive), so nothing under core/tests runs docker
// itself any more and the rule holds with no exceptions.
var Ledger = []LedgerEntry{
	{File: "internal/transport/rclone/connections_gate_test.go", Rule: RuleUnitReachesContainer, Issue: 448},
	{File: "internal/transport/rclone/dockerbuild_test.go", Rule: RuleUnitReachesContainer, Issue: 448},
	{File: "internal/transport/rclone/errors_test.go", Rule: RuleUnitReachesContainer, Issue: 448},
	{File: "internal/transport/rclone/gate_test.go", Rule: RuleUnitReachesContainer, Issue: 448},
	{File: "internal/transport/rclone/ssh_test.go", Rule: RuleUnitReachesContainer, Issue: 448},
	{File: "service/backupsets_docker_test.go", Rule: RuleUnitReachesContainer, Issue: 448},
}

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
