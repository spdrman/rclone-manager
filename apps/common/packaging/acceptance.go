package packaging

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// The acceptance procedures in docs/acceptance/ are the substitute for
// hardware nobody can reach from a laptop, which makes their instructions
// carry the same weight as code: an operator runs them as root, once, on a
// NAS that may already hold their data. Two of those instructions are
// checkable from here.
//
// The first is destructive: a recursive chown across a share the procedure
// only mkdir -p'd rewrites the ownership of everything another tool put
// there, and is not reversible without an ownership record nobody took.
//
// The second is the phase's own central data-safety criterion. Every
// procedure asks the operator to confirm the backup root is "untouched,
// byte for byte" after removal. With no baseline recorded before, that box
// gets ticked off a directory listing, and a partial deletion or a
// truncated artifact passes. A criterion that cannot fail is worse than an
// unclaimed one, because it stops anyone looking.

const (
	// RuleRecursiveChown is a `chown -R` that reaches the backup root or
	// an ancestor of it.
	RuleRecursiveChown = "recursive-chown-on-operator-data"
	// RuleUnverifiableClaim is a removal-safety claim with no recorded
	// baseline to check it against.
	RuleUnverifiableClaim = "unverifiable-removal-claim"
	// RuleBaselineInsideBackupRoot is a canary hash or file listing
	// recorded inside the very tree it is meant to vouch for.
	RuleBaselineInsideBackupRoot = "baseline-inside-the-backup-root"
)

var (
	recursiveChownRe = regexp.MustCompile(`(?m)^\s*chown\s+(?:-[a-zA-Z]*R[a-zA-Z]*|--recursive)\b(.*)$`)
	teeTargetRe      = regexp.MustCompile(`(?:\|\s*tee\s+|>\s*)("?[$/][^\s"'|]*"?)`)
	untouchedClaimRe = regexp.MustCompile(`(?i)untouched,?\s+byte\s+for\s+byte`)
)

// AcceptanceEvidence are the markers of a procedure that recorded
// something before it asked the operator to confirm nothing changed. They
// are deliberately the same shape the Synology package lifecycle procedure
// already uses, rather than a new invention.
var acceptanceEvidence = []struct {
	marker string
	need   string
}{
	{"/dev/urandom", "write a canary file of known content into the backup root"},
	{"sha256sum", "record the canary's hash"},
	{"sha256sum -c", "verify the canary hash after removal"},
	{"find ", "record a full file listing of the backup root"},
	{"diff ", "compare the recorded listing against the tree after removal"},
}

// CheckAcceptanceProcedure holds one acceptance procedure to the two rules
// that are decidable from its text. backupRoot is the platform's declared
// backup root, and subs expands the placeholders the procedure writes in
// place of machine-specific paths ("$DISK", "/mnt/POOL"), because a rule
// that only understands literal paths would pass every document that uses
// a variable, which is all three of them.
func CheckAcceptanceProcedure(text, backupRoot string, subs map[string]string) []Violation {
	var out []Violation

	resolve := func(token string) string {
		t := strings.Trim(strings.TrimSpace(token), `"'`)
		t = strings.ReplaceAll(t, "${DISK}", "$DISK")
		for from, to := range subs {
			t = strings.ReplaceAll(t, from, to)
		}
		return t
	}

	for _, m := range recursiveChownRe.FindAllStringSubmatch(text, -1) {
		for _, token := range strings.Fields(m[1]) {
			path := resolve(token)
			if !strings.HasPrefix(path, "/") {
				continue
			}
			if Contains(path, backupRoot) {
				out = append(out, Violation{"chown -R", RuleRecursiveChown,
					fmt.Sprintf("recursively chowns %s, which is the backup root %s or an ancestor of it; the share may already hold data another tool wrote, and rewriting its ownership is not reversible without a record nobody took",
						backquote(path), backquote(backupRoot))})
			}
		}
	}

	if untouchedClaimRe.MatchString(text) {
		for _, e := range acceptanceEvidence {
			if !strings.Contains(text, e.marker) {
				out = append(out, Violation{"removal criterion", RuleUnverifiableClaim,
					fmt.Sprintf("claims the backup root is untouched byte for byte but never does one thing: %s (no %s anywhere in the procedure)", e.need, backquote(strings.TrimSpace(e.marker)))})
			}
		}
	}

	// Only the recording lines are checked, never every redirection in
	// the document: the canary itself is written inside the backup root
	// on purpose, and it is the hash and the listing that have to survive
	// whatever happens to that tree.
	for _, line := range strings.Split(text, "\n") {
		if !strings.Contains(line, "sha256sum") && !strings.Contains(line, "find ") {
			continue
		}
		for _, m := range teeTargetRe.FindAllStringSubmatch(line, -1) {
			target := resolve(m[1])
			if !strings.HasPrefix(target, "/") {
				continue
			}
			if Contains(backupRoot, target) {
				out = append(out, Violation{"baseline record", RuleBaselineInsideBackupRoot,
					fmt.Sprintf("records %s inside the backup root %s, so whatever damaged the tree can have damaged the evidence too",
						backquote(target), backquote(backupRoot))})
			}
		}
	}

	sortViolations(out)
	return out
}

// ReadAcceptanceProcedure is CheckAcceptanceProcedure over a file.
func ReadAcceptanceProcedure(path, backupRoot string, subs map[string]string) ([]Violation, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return CheckAcceptanceProcedure(string(data), backupRoot, subs), nil
}
