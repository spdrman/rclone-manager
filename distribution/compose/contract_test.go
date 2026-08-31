package compose_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/distribution/compose"
)

// env is what an operator's .env supplies. The canonical definition uses
// the ${VAR:?message} form for every host path precisely so an unset
// variable stops the deployment instead of landing somewhere plausible, so
// a contract check has to supply them the way `docker compose` would.
func env() map[string]string {
	return map[string]string{
		"STATE_DIR":        "/srv/backup-manager/state",
		"BACKUP_DIR":       "/srv/backup-manager/backups",
		"CONFIG_FILE":      "/srv/backup-manager/config/config.yaml",
		"SSH_KEY_FILE":     "/srv/backup-manager/secrets/id_ed25519",
		"KNOWN_HOSTS_FILE": "/srv/backup-manager/secrets/known_hosts",
	}
}

func canonical(t *testing.T) compose.Document {
	t.Helper()
	c := compose.MustLoadContract()
	doc, err := compose.Read(compose.Path(c.Canonical), env())
	if err != nil {
		t.Fatalf("read the canonical runtime definition: %v", err)
	}
	return doc
}

// ---------------------------------------------------------------------
// Every required field is declared
// ---------------------------------------------------------------------

func TestCanonicalDefinitionDeclaresEveryRequiredField(t *testing.T) {
	t.Parallel()

	c := compose.MustLoadContract()
	doc := canonical(t)

	for _, field := range c.Fields {
		t.Run(field.ID, func(t *testing.T) {
			t.Parallel()
			if findings := doc.CheckField(field); len(findings) != 0 {
				for _, f := range findings {
					t.Errorf("%s: %s\n  why the contract requires it: %s", f.Rule, f.Detail, field.Why)
				}
			}
		})
	}
}

// TestRequiredFieldCheckFailsWhenAFieldIsRemoved is the positive control
// for the suite above, one mutation per field. Without it a CheckField
// that returned nil unconditionally would report a complete contract
// against an empty file.
func TestRequiredFieldCheckFailsWhenAFieldIsRemoved(t *testing.T) {
	t.Parallel()

	c := compose.MustLoadContract()
	data, err := os.ReadFile(compose.Path(c.Canonical))
	if err != nil {
		t.Fatalf("read canonical: %v", err)
	}

	for _, field := range c.Fields {
		t.Run(field.ID, func(t *testing.T) {
			t.Parallel()

			doc, err := compose.Parse(data, c.Canonical, env())
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			mutated, what := doc.WithoutField(field)
			if what == "" {
				t.Fatalf("nothing to remove for %q, so this control proves nothing", field.ID)
			}
			findings := mutated.CheckField(field)
			if len(findings) == 0 {
				t.Fatalf("removing %s did not fail the %q check, so that check cannot detect a missing field", what, field.ID)
			}
			joined := findingText(findings)
			if !strings.Contains(joined, field.ID) {
				t.Errorf("the finding %q does not name the rule %q that produced it", joined, field.ID)
			}
		})
	}
}

func findingText(findings []compose.Finding) string {
	parts := make([]string, 0, len(findings))
	for _, f := range findings {
		parts = append(parts, f.Rule+": "+f.Detail)
	}
	return strings.Join(parts, " | ")
}

// ---------------------------------------------------------------------
// No prohibited host privilege, anywhere
// ---------------------------------------------------------------------

func TestNoDefinitionNeedsAProhibitedHostPrivilege(t *testing.T) {
	t.Parallel()

	c := compose.MustLoadContract()
	paths := append([]string{c.Canonical}, c.Derived...)

	for _, rel := range paths {
		t.Run(rel, func(t *testing.T) {
			t.Parallel()
			doc, err := compose.Read(compose.Path(rel), env())
			if err != nil {
				t.Fatalf("read %s: %v", rel, err)
			}
			for _, f := range doc.CheckProhibited(c) {
				t.Errorf("%s\n  %s: %s\n  why it is prohibited: %s", rel, f.Rule, f.Detail, f.Why)
			}
		})
	}
}

// TestProhibitionCheckFiresOnEveryProhibitedSetting is the positive
// control: the canonical definition, mutated to require each prohibited
// setting in turn, must be rejected and must say which service and which
// setting. Three preflight rules in this repository have already failed
// open; a prohibition list nobody has watched fail is a list of comments.
func TestProhibitionCheckFiresOnEveryProhibitedSetting(t *testing.T) {
	t.Parallel()

	c := compose.MustLoadContract()
	data, err := os.ReadFile(compose.Path(c.Canonical))
	if err != nil {
		t.Fatalf("read canonical: %v", err)
	}

	for _, rule := range c.Prohibited {
		t.Run(rule.ID, func(t *testing.T) {
			t.Parallel()

			doc, err := compose.Parse(data, c.Canonical, env())
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			mutated, what := doc.WithProhibited(rule)
			if what == "" {
				t.Fatalf("no mutation is defined for %q, so this control proves nothing", rule.ID)
			}

			findings := mutated.CheckProhibited(c)
			if len(findings) == 0 {
				t.Fatalf("injecting %s did not trip the %q rule, so that rule fails open", what, rule.ID)
			}

			var matched *compose.Finding
			for i := range findings {
				if findings[i].Rule == rule.ID {
					matched = &findings[i]
					break
				}
			}
			if matched == nil {
				t.Fatalf("injecting %s tripped %s, but not %q itself: a rule that only fires through another rule is not the rule it claims to be", what, findingText(findings), rule.ID)
			}
			if matched.Service == "" {
				t.Errorf("the %q finding does not name the service it was found on: %q", rule.ID, matched.Detail)
			}
			if !strings.Contains(matched.Detail, what) {
				t.Errorf("the %q finding says %q, which never mentions the injected value %q", rule.ID, matched.Detail, what)
			}
		})
	}
}

// TestProhibitionScanSeesKeysTheParserHasNoFieldFor guards the specific
// way a checker like this fails open: parsing compose into a struct means
// a key with no matching field is invisible, so `privileged: true` on a
// service the struct never modelled would read as clean.
func TestProhibitionScanSeesKeysTheParserHasNoFieldFor(t *testing.T) {
	t.Parallel()

	const doc = `
services:
  something-nobody-modelled:
    image: backup-manager:dev
    command: ["/backup-manager-web", "serve"]
    privileged: true
    x-invented-key:
      nested:
        network_mode: host
`
	c := compose.MustLoadContract()
	parsed, err := compose.Parse([]byte(doc), "synthetic.yaml", env())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	findings := parsed.CheckProhibited(c)
	if len(findings) < 2 {
		t.Fatalf("the scan found %d finding(s) in a document that declares privileged mode AND host networking under an unmodelled key: %s", len(findings), findingText(findings))
	}
}

// ---------------------------------------------------------------------
// Completeness: nothing derives from the canonical definition unchecked
// ---------------------------------------------------------------------

func TestEveryComposeArtifactInTheTreeIsRegistered(t *testing.T) {
	t.Parallel()

	c := compose.MustLoadContract()
	registered := map[string]bool{c.Canonical: true}
	for _, d := range c.Derived {
		registered[d] = true
	}

	var found []string
	roots := []string{"apps", "container", "distribution"}
	for _, root := range roots {
		err := filepath.WalkDir(compose.Path(root), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			ext := filepath.Ext(path)
			if ext != ".yaml" && ext != ".yml" {
				return nil
			}
			body, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			if !strings.Contains(string(body), "\nservices:") && !strings.HasPrefix(string(body), "services:") {
				return nil
			}
			rel, _ := filepath.Rel(compose.Path("."), path)
			found = append(found, filepath.ToSlash(rel))
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}

	if len(found) == 0 {
		t.Fatal("the walk found no compose artifact at all, so this completeness check is checking nothing")
	}
	for _, rel := range found {
		if !registered[rel] {
			t.Errorf("%s declares compose services but is not registered in runtime-contract.json, so no prohibition rule ever runs against it", rel)
		}
	}
}

// ---------------------------------------------------------------------
// Roles come from the command, never from the service name
// ---------------------------------------------------------------------

func TestServiceRolesAreDerivedFromTheCommand(t *testing.T) {
	t.Parallel()

	doc := canonical(t)
	roles := map[compose.Role][]string{}
	for name, role := range doc.Roles() {
		roles[role] = append(roles[role], name)
	}

	for _, want := range []compose.Role{compose.RoleEngine, compose.RoleWebUI} {
		if len(roles[want]) != 1 {
			t.Errorf("the canonical definition has %d service(s) in the %q role, want exactly 1: %v", len(roles[want]), want, roles[want])
		}
	}

	// Positive control: renaming both services must not change the roles,
	// because a check keyed on the name would silently stop checking.
	renamed, err := compose.Parse([]byte(`
services:
  totally-different-name:
    image: backup-manager:dev
    command: ["/backup-manager-web", "serve", "--profile=generic"]
  another-name-entirely:
    image: backup-manager:dev
    command: ["/backup-manager-web", "serve-ui", "--profile=generic"]
`), "synthetic.yaml", env())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := renamed.Roles()
	if got["totally-different-name"] != compose.RoleEngine {
		t.Errorf("a renamed engine resolved to role %q, want %q", got["totally-different-name"], compose.RoleEngine)
	}
	if got["another-name-entirely"] != compose.RoleWebUI {
		t.Errorf("a renamed web UI resolved to role %q, want %q", got["another-name-entirely"], compose.RoleWebUI)
	}
}

// ---------------------------------------------------------------------
// Private state and backup data never contain one another
// ---------------------------------------------------------------------

func TestPrivateStateAndBackupDataAreSeparateMounts(t *testing.T) {
	t.Parallel()

	doc := canonical(t)
	state, ok := doc.MountFor(compose.RoleEngine, "/data/state")
	if !ok {
		t.Fatal("the engine declares no /data/state mount")
	}
	backups, ok := doc.MountFor(compose.RoleEngine, "/data/backups")
	if !ok {
		t.Fatal("the engine declares no /data/backups mount")
	}

	if state.HostPath == backups.HostPath {
		t.Fatalf("private state and backup data share the host path %q", state.HostPath)
	}
	if compose.Contains(backups.HostPath, state.HostPath) {
		t.Errorf("private application state %q lives inside the backup destination %q, so a backup share handed to a user carries the state database and the local-auth record", state.HostPath, backups.HostPath)
	}
	if compose.Contains(state.HostPath, backups.HostPath) {
		t.Errorf("the backup destination %q lives inside the private state directory %q", backups.HostPath, state.HostPath)
	}

	// Positive control: the containment helper has to actually detect
	// containment, or the two assertions above pass for any input.
	if !compose.Contains("/srv/backups", "/srv/backups/state") {
		t.Error("Contains fails to see a directory inside its own parent, so the separation assertions above are vacuous")
	}
	if compose.Contains("/srv/backups", "/srv/backups-elsewhere") {
		t.Error("Contains treats a sibling with a shared prefix as contained, which would make it fire on correct layouts")
	}
}
