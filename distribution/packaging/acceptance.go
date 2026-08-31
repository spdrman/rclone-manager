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
	// RuleMissingConfigPrecondition is an install procedure that never
	// asks for a config.yaml before the step that starts the engine.
	RuleMissingConfigPrecondition = "missing-config-precondition"
)

var (
	// A privilege prefix is part of the command, not a way past this
	// rule: every one of these procedures runs its ownership fix-up as
	// root, and the Proxmox one does it over ssh, where `sudo` is how
	// that happens. Anchoring on a bare `chown` let `sudo chown -R` over
	// the backup root's parent through unseen.
	recursiveChownRe = regexp.MustCompile(`(?m)^\s*(?:(?:sudo|doas)\s+)?chown\s+(?:-[a-zA-Z]*R[a-zA-Z]*|--recursive)\b(.*)$`)
	teeTargetRe      = regexp.MustCompile(`(?:\|\s*tee\s+|>\s*)("?[$/][^\s"'|]*"?)`)
	untouchedClaimRe = regexp.MustCompile(`(?i)untouched,?\s+byte\s+for\s+byte`)

	installStepRe     = regexp.MustCompile(`(?m)^##\s+Step\s+1\b`)
	configRefusalRe   = regexp.MustCompile(`(?i)hard start(?:up)? failure|refuses? to start`)
	configIssueRe     = regexp.MustCompile(`#176\b`)
	configChecklistRe = regexp.MustCompile(`(?mi)^\s*-\s*\[[ x]\][^\n]*config\.yaml`)
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

// A third instruction is checkable from here, and it is the one a fresh
// install fails on first.
//
// Every adapter gives the engine `healthcheck: ["CMD", "/backup-manager",
// "status"]` and gates the only container that publishes a port on it
// with `depends_on: condition: service_healthy`, because
// container/compose.yaml does and EquivalenceProperties compares both.
// `status` opens the service, `core/service.Open` loads and validates
// config.yaml, and with no config file on disk it exits non-zero: the
// engine never turns healthy, and `docker compose up` aborts the UI
// container. So a procedure that says "both containers reach running"
// and "the published port loads the web UI" without having asked for a
// config.yaml first states criteria its own step 1 cannot reach, and the
// operator is stopped before the destructive-safety re-check the whole
// procedure exists to produce.
//
// Removing that refusal is #176's work and is not merged. Until it is,
// the interim handling #176 itself names is to make writing a config
// step 0 of every acceptance procedure, which is what #175 did for the
// five procedures #202 converted. This rule is that handling, checked.

// CheckConfigPrecondition holds an install procedure to the config.yaml
// precondition: before the install step, in the prerequisites where an
// operator still has a shell, the procedure has to ask for the file, say
// why refusing to write it is a startup failure rather than a first-run
// wizard, name #176 as the reason the step exists at all, and carry a
// checklist box the operator can tick.
//
// The rule fails closed on a document with no `## Step 1` heading, rather
// than reporting a clean prelude that is the whole file.
func CheckConfigPrecondition(text string) []Violation {
	var out []Violation
	add := func(detail string) {
		out = append(out, Violation{"step 0", RuleMissingConfigPrecondition, detail})
	}

	loc := installStepRe.FindStringIndex(text)
	if loc == nil {
		add("has no `## Step 1` heading, so nothing in it can be shown to come before the install")
		return out
	}
	prelude := text[:loc[0]]

	if !strings.Contains(prelude, "config.yaml") {
		add("never names `config.yaml` before the install step, and the engine's healthcheck runs `status`, which exits non-zero until a valid config exists: the stack cannot reach the running and healthy state this procedure then asks the operator to confirm")
	}
	if !configRefusalRe.MatchString(prelude) {
		add("does not say that a missing or invalid config is a hard startup failure rather than a first-run wizard, which is the one sentence that stops an operator clicking install and waiting for a wizard that never comes")
	}
	if !configIssueRe.MatchString(prelude) {
		add("does not cite #176 as the reason this step exists, so nobody reading it later can tell which part becomes optional once the engine serves a first-run flow")
	}
	if !configChecklistRe.MatchString(prelude) {
		add("has no checklist box naming `config.yaml`, so the precondition is prose an operator can read past rather than a step they tick")
	}

	sortViolations(out)
	return out
}

// ReadConfigPrecondition is CheckConfigPrecondition over a file.
func ReadConfigPrecondition(path string) ([]Violation, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return CheckConfigPrecondition(string(data)), nil
}
