package compose_test

import (
	"encoding/json"
	"encoding/xml"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/spdrman/rclone-manager/distribution/compose"
)

// separation_test.go is issue #87 (B5.1)'s state-separation regression
// suite: "private state, credentials and host keys are proven separate
// from backup data on EVERY claimed platform".
//
// contract_test.go's TestPrivateStateAndBackupDataAreSeparateMounts
// already proves it, and proves it well, for the canonical definition.
// The canonical definition is not a claimed platform, it is the thing
// claimed platforms derive from, and a derivation is exactly where a
// mount gets re-pointed at whatever host path that NAS OS happens to hand
// out. This runs the same rule over every derived artifact the contract
// registers, so an adapter cannot land a layout that nests the local-auth
// record or the SSH private key inside a share an operator hands to a
// user.
//
// The rule is written as "whatever an artifact DOES declare must not nest
// with the backup destination", not as "every artifact must declare all of
// these". Requiring the full field set of a derived artifact would make it
// a second definition rather than a derivation, which is the asymmetry
// runtime-contract.json is explicit about.

// privatePaths are the container paths that must never live inside the
// backup destination. Each one holds something an operator would not
// knowingly publish: the lifecycle journal and the local-auth Argon2id
// record, the manager's configuration, the SFTP private key, and the
// pinned host keys that are the whole of this product's MITM defence.
var privatePaths = []struct {
	containerPath string
	what          string
}{
	{"/data/state", "the lifecycle journal and the local-auth administrator record"},
	// The DIRECTORY, not config.yaml inside it. Issue #196 turned the
	// packaged configuration mount from a read-only single file into a
	// writable directory, because the engine creates and atomically
	// replaces that file and keeps the ssh_keys/ and known_hosts.d/
	// stores beside it. The directory is therefore what every artifact
	// declares, and it is also the stricter thing to check: nesting it
	// inside the backup destination would publish the two key stores as
	// well as the configuration.
	{"/etc/backup-manager/config", "the manager's configuration directory"},
	{"/etc/backup-manager/id_ed25519", "the SFTP private key"},
	{"/etc/backup-manager/known_hosts", "the pinned host keys"},
}

const backupDataPath = "/data/backups"

// separationEnv is contract_test.go's env() plus every other variable a
// registered artifact interpolates into a host path.
//
// Supplying them is not cosmetic. compose.Document.Mounts splits a
// short-syntax volume on ":", so an unexpanded ${DISK:?set DISK in ...},
// whose default-message half contains colons of its own, parses into a
// host path that matches no container path at all, and every mount rule
// keyed on a container path then finds nothing and reports nothing. That
// is a silent fail-open, which is why the suite below FAILS rather than
// skips when an artifact yields no backup destination.
func separationEnv() map[string]string {
	out := map[string]string{
		"DISK":     "/srv/dev-disk-by-uuid-0000",
		"KEY_FILE": "/srv/backup-manager/secrets/id_ed25519",
	}
	for k, v := range env() {
		out[k] = v
	}
	return out
}

// ---------------------------------------------------------------------
// Reading an artifact honestly
// ---------------------------------------------------------------------

// helmValuesFor names the values file an artifact's Helm placeholders are
// expanded from before this suite reads it.
//
// apps/truenas/catalog/templates/docker-compose.yaml declares its host
// paths as `{{ .Values.storage.state.hostPath }}` and friends, and this
// suite used to compare those strings verbatim: the equality test and
// both Contains comparisons could never match, so the TrueNAS cell was
// reported as PASS while checking nothing at all (issue #87's review,
// M8). ix_values.yaml carries the defaults every one of those questions
// ships with, which is exactly the layout an operator gets by clicking
// through, so expanding against it is reading the artifact rather than
// inventing one.
var helmValuesFor = map[string]string{
	"apps/truenas/catalog/templates/docker-compose.yaml": "apps/truenas/catalog/ix_values.yaml",
}

var helmPlaceholder = regexp.MustCompile(`{{\s*\.Values\.([A-Za-z0-9_.]+)\s*}}`)

// unresolvedIn reports the template or variable markers a host path still
// carries. A non-empty answer means the path was never resolved, and a
// comparison against it proves nothing: the whole point of the rule below
// is that two REAL host paths do not nest.
func unresolvedIn(hostPath string) []string {
	var out []string
	for _, marker := range []string{"{{", "}}", "${", "$("} {
		if strings.Contains(hostPath, marker) {
			out = append(out, marker)
		}
	}
	return out
}

// separationDoc reads one registered artifact with every substitution
// this suite knows how to make already applied.
func separationDoc(t *testing.T, rel string) compose.Document {
	t.Helper()
	raw, err := os.ReadFile(compose.Path(rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	if valuesRel, ok := helmValuesFor[rel]; ok {
		raw = expandHelmValues(t, raw, valuesRel)
	}
	doc, err := compose.Parse(raw, rel, separationEnv())
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	return doc
}

// expandHelmValues substitutes `{{ .Values.a.b.c }}` from a chart's own
// values file. A placeholder with no value is left exactly as it is, so
// it reaches the unresolved-marker check below and fails loudly instead
// of quietly becoming an empty string that matches nothing.
func expandHelmValues(t *testing.T, raw []byte, valuesRel string) []byte {
	t.Helper()
	valuesRaw, err := os.ReadFile(compose.Path(valuesRel))
	if err != nil {
		t.Fatalf("read %s: %v", valuesRel, err)
	}
	var values map[string]any
	if err := yaml.Unmarshal(valuesRaw, &values); err != nil {
		t.Fatalf("parse %s: %v", valuesRel, err)
	}

	substituted := 0
	out := helmPlaceholder.ReplaceAllFunc(raw, func(match []byte) []byte {
		key := helmPlaceholder.FindSubmatch(match)[1]
		var node any = values
		for _, seg := range strings.Split(string(key), ".") {
			m, ok := node.(map[string]any)
			if !ok {
				return match
			}
			if node, ok = m[seg]; !ok {
				return match
			}
		}
		text, ok := node.(string)
		if !ok {
			return match
		}
		substituted++
		return []byte(text)
	})
	if substituted == 0 {
		t.Fatalf("%s substituted no placeholder at all, so the artifact it was supposed to resolve is still unresolved", valuesRel)
	}
	return out
}

// ---------------------------------------------------------------------
// Which platforms this suite actually reads
// ---------------------------------------------------------------------

// platformOfArtifact maps a registered compose artifact to the claimed
// platform it belongs to. The canonical definition is the generic
// profile's own deployment; everything else lives under apps/<platform>.
func platformOfArtifact(rel string) string {
	if after, ok := strings.CutPrefix(rel, "apps/"); ok {
		return strings.SplitN(after, "/", 2)[0]
	}
	return "generic"
}

// nonComposePlatformCoverage names a claimed platform this suite checks
// through something other than a Compose document, and where.
var nonComposePlatformCoverage = map[string]string{
	"unraid": "apps/unraid/template/backup-manager.xml, read by TestTheUnraidTemplateKeepsPrivateStateOutOfTheBackupShare",
}

// platformsWithNoHostPathsToCheck names a claimed platform this suite
// deliberately does not read, and why. An entry here is the gap made
// visible: it is a claim somebody has to defend, not an absence nobody
// notices.
var platformsWithNoHostPathsToCheck = map[string]string{
	"ugos":     "kind \"none\" in conformance.json: UGOS ships no package or deployment artifact of its own, so there is no host-path layout for this rule to read. It runs the canonical Compose deployment, which IS checked here as the generic profile.",
	"synology": "kind \"spk\": DSM chooses the paths, not a file this suite can read. Private state lives in the per-package FHS tree (/var/packages/<pkg>/var, /etc, /home - apps/synology/spk/layout.go) and backup data in the DSM shared folder the data-share resource worker creates, which are structurally disjoint trees rather than two operator-editable paths that could be pointed at each other. apps/synology/spk/lifecycle_test.go is where that split is asserted, in that module.",
}

// claimedPlatform is one provider as the conformance matrix declares it:
// its id, and the runtime definition it says it deploys.
//
// The compose path is read as well as the id because a platform does not
// have to own its artifact. Dockge deploys container/compose.yaml, the
// canonical definition, and says so in that field; its host-path layout
// therefore IS read by this suite, under the canonical file's own name,
// and recording that as an exemption would have described the same bytes
// as unchecked.
type claimedPlatform struct {
	ID      string
	Compose string
}

// claimedPlatforms reads the provider set the product actually claims,
// from the same file the conformance matrix is built out of.
func claimedPlatforms(t *testing.T) []claimedPlatform {
	t.Helper()
	raw, err := os.ReadFile(compose.Path("distribution/packaging/conformance.json"))
	if err != nil {
		t.Fatalf("read conformance.json: %v", err)
	}
	var doc struct {
		Providers map[string]struct {
			Metadata struct {
				Kind    string `json:"kind"`
				Root    string `json:"root"`
				Compose string `json:"compose"`
			} `json:"metadata"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse conformance.json: %v", err)
	}
	out := make([]claimedPlatform, 0, len(doc.Providers))
	for id, p := range doc.Providers {
		rel := p.Metadata.Compose
		// A compose path is written relative to the provider's own root
		// everywhere except where it names a file outside that root,
		// which is the whole point of the field for a platform that
		// deploys somebody else's definition.
		if rel != "" && !strings.HasPrefix(rel, p.Metadata.Root+"/") {
			if _, err := os.Stat(compose.Path(rel)); err != nil {
				rel = p.Metadata.Root + "/" + rel
			}
		}
		out = append(out, claimedPlatform{ID: id, Compose: rel})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// TestEveryClaimedPlatformIsReadBySomethingHere is the guard the previous
// version of this file did not have. Its non-vacuity check counted
// REGISTERED COMPOSE ARTIFACTS, so two claimed platforms could sit
// entirely outside the suite (Unraid's template and Synology's SPK did)
// while the acceptance criterion this file carries says "every claimed
// platform". Counting artifacts cannot notice a missing platform; only
// comparing against the claimed set can.
func TestEveryClaimedPlatformIsReadBySomethingHere(t *testing.T) {
	t.Parallel()

	c := compose.MustLoadContract()
	read := map[string]bool{}
	covered := map[string]string{}
	for _, rel := range append([]string{c.Canonical}, c.Derived...) {
		read[rel] = true
		covered[platformOfArtifact(rel)] = rel
	}

	claimed := claimedPlatforms(t)
	if len(claimed) < 2 {
		t.Fatalf("conformance.json declares %d provider(s); this guard proves nothing below two", len(claimed))
	}

	ids := make([]string, 0, len(claimed))
	for _, p := range claimed {
		ids = append(ids, p.ID)
		switch {
		case covered[p.ID] != "":
		// A platform that declares a runtime definition this suite
		// already reads is covered by it, whoever registered that
		// artifact. This is coverage and not an exemption: the file
		// whose host paths get checked is the file this platform says
		// it deploys, byte for byte.
		case p.Compose != "" && read[p.Compose]:
		case nonComposePlatformCoverage[p.ID] != "":
		case platformsWithNoHostPathsToCheck[p.ID] != "":
		default:
			t.Errorf("platform %q is claimed in conformance.json and nothing in this suite reads its host-path layout.\n"+
				"Issue #87's acceptance criterion is that private state, credentials and host keys are proven separate from backup data on EVERY claimed platform. Either read its artifact here, or add an entry to platformsWithNoHostPathsToCheck saying why there is nothing to read.", p.ID)
		}
	}

	// Stale entries are the other direction of the same failure: an
	// exemption for a platform nobody claims any more reads as coverage.
	for id := range platformsWithNoHostPathsToCheck {
		if !slices.Contains(ids, id) {
			t.Errorf("platformsWithNoHostPathsToCheck names %q, which conformance.json does not claim", id)
		}
	}
	for id := range nonComposePlatformCoverage {
		if !slices.Contains(ids, id) {
			t.Errorf("nonComposePlatformCoverage names %q, which conformance.json does not claim", id)
		}
	}
}

// TestDeclaredArtifactCoverageIsRealCoverage is the control for the
// clause above, and it is the clause that needed one: reading coverage
// out of a platform's own declaration is one typo away from reading it
// out of nothing at all, and a platform silently covered by an empty
// string looks exactly like a platform genuinely covered.
//
// Two directions, because either one alone passes while the rule is
// broken. A platform whose declared definition is one this suite reads
// has to resolve to that definition and no other, and a platform whose
// declared definition is NOT read has to stay uncovered.
func TestDeclaredArtifactCoverageIsRealCoverage(t *testing.T) {
	t.Parallel()

	c := compose.MustLoadContract()
	read := map[string]bool{}
	for _, rel := range append([]string{c.Canonical}, c.Derived...) {
		read[rel] = true
	}

	owned := map[string]bool{}
	for _, rel := range append([]string{c.Canonical}, c.Derived...) {
		owned[platformOfArtifact(rel)] = true
	}

	claimed := claimedPlatforms(t)
	dependsOnTheClause := 0
	for _, p := range claimed {
		if p.Compose != "" {
			if _, err := os.Stat(compose.Path(p.Compose)); err != nil {
				t.Errorf("platform %q declares compose %q, which is not in the tree: %v.\n"+
					"A declared path nobody resolves is how the coverage clause turns into a rubber stamp", p.ID, p.Compose, err)
				continue
			}
		}
		// The platforms the clause exists for: no artifact of their own
		// under apps/<id>/, no entry in either exemption map, and so
		// covered by the file they say they deploy or by nothing.
		if owned[p.ID] || nonComposePlatformCoverage[p.ID] != "" || platformsWithNoHostPathsToCheck[p.ID] != "" {
			continue
		}
		dependsOnTheClause++
		if p.Compose == "" {
			t.Errorf("platform %q is covered by nothing but its declared compose path and declares none", p.ID)
			continue
		}
		if !read[p.Compose] {
			t.Errorf("platform %q is covered by nothing but its declared compose path %q, and this suite does not read that file", p.ID, p.Compose)
		}
	}
	if dependsOnTheClause == 0 {
		t.Fatalf("no claimed platform depends on the declared-artifact coverage clause, so it is inert and TestEveryClaimedPlatformIsReadBySomethingHere would pass with it deleted")
	}

	// The negative direction. A platform pointed at a real file that is
	// not a registered runtime artifact must not be counted as covered.
	unread := "container/.env.example"
	if _, err := os.Stat(compose.Path(unread)); err != nil {
		t.Fatalf("the control needs a real file this suite does not read: %v", err)
	}
	if read[unread] {
		t.Fatalf("%s is now a registered runtime artifact, so it cannot be the control for a path this suite does not read", unread)
	}
}

// ---------------------------------------------------------------------
// The rule itself
// ---------------------------------------------------------------------

// checkSeparation applies the whole rule to one artifact's engine mounts,
// however they were read. mountFor answers a container path.
//
// It is shared by the Compose walk and the Unraid template so the two
// cannot drift into checking different things.
func checkSeparation(t *testing.T, source string, mountFor func(containerPath string) (compose.Mount, bool)) int {
	t.Helper()

	backups, ok := mountFor(backupDataPath)
	if !ok {
		t.Fatalf("%s yields no engine mount at %s, so every rule below silently checks nothing on this artifact.\n"+
			"Either the adapter genuinely does not mount backup data, or its volume line did not parse (an unexpanded ${VAR:?message} splits on the colons in its own message)",
			source, backupDataPath)
	}
	if markers := unresolvedIn(backups.HostPath); len(markers) > 0 {
		t.Fatalf("%s: the backup destination is %q, which still carries %v, so it was never resolved to a host path and nothing below can be compared against it",
			source, backups.HostPath, markers)
	}

	compared := 0
	for _, p := range privatePaths {
		mount, declared := mountFor(p.containerPath)
		if !declared {
			// Not a silent continue any more (issue #87's review, M8).
			// An artifact that contributes nothing used to pass, and the
			// only fail-open guard covered the backup destination.
			if reason := privatePathAbsences[source+"|"+p.containerPath]; reason != "" {
				continue
			}
			t.Errorf("%s declares no engine mount at %s, so %s was never compared against the backup destination.\n"+
				"Either the artifact stopped declaring it, or its volume line did not parse. If this adapter genuinely does not mount it, add an entry to privatePathAbsences with the reason.", source, p.containerPath, p.what)
			continue
		}
		if markers := unresolvedIn(mount.HostPath); len(markers) > 0 {
			t.Errorf("%s: %s is mounted from %q, which still carries %v, so it was never resolved to a host path and comparing it against the backup destination proves nothing",
				source, p.containerPath, mount.HostPath, markers)
			continue
		}

		compared++
		if mount.HostPath == backups.HostPath {
			t.Errorf("%s: %s and the backup destination share the host path %q", p.what, p.containerPath, mount.HostPath)
			continue
		}
		if compose.Contains(backups.HostPath, mount.HostPath) {
			t.Errorf("%s lives at %q, inside the backup destination %q: %s",
				p.what, mount.HostPath, backups.HostPath, p.containerPath)
		}
		if compose.Contains(mount.HostPath, backups.HostPath) {
			t.Errorf("the backup destination %q lives inside %q, which holds %s",
				backups.HostPath, mount.HostPath, p.what)
		}
	}

	if compared == 0 {
		t.Errorf("%s declared a backup destination and not one private path, so this artifact was reported as checked while comparing nothing", source)
	}
	return compared
}

// privatePathAbsences names, per artifact, one private container path
// that artifact legitimately does not mount, and why. Keyed
// "<artifact>|<container path>".
//
// Empty today: every registered artifact mounts all four. It exists so
// that an adapter which genuinely does not (a platform that keeps host
// keys somewhere this rule cannot see, say) has to write the reason down
// rather than disappear from the comparison silently.
var privatePathAbsences = map[string]string{}

func TestPrivateStateIsSeparateFromBackupDataOnEveryClaimedPlatform(t *testing.T) {
	t.Parallel()

	c := compose.MustLoadContract()
	artifacts := append([]string{c.Canonical}, c.Derived...)
	if len(artifacts) < 2 {
		t.Fatalf("the contract registers %d artifact(s); this suite proves nothing about derived adapters below two", len(artifacts))
	}

	var checked atomic.Int64
	for _, rel := range artifacts {
		t.Run(rel, func(t *testing.T) {
			t.Parallel()

			doc := separationDoc(t, rel)
			checked.Add(int64(checkSeparation(t, rel, func(containerPath string) (compose.Mount, bool) {
				return doc.MountFor(compose.RoleEngine, containerPath)
			})))
		})
	}

	t.Cleanup(func() {
		if checked.Load() == 0 {
			t.Error("no artifact declared any private mount alongside a backup destination, so this suite compared nothing")
		}
	})
}

// TestTheSeparationRuleWouldNoticeANestedLayout is the positive control.
// Without it, a Contains that never reported containment (or a MountFor
// that never found a mount) would make the suite above pass on a layout
// that puts the SSH private key inside the backup share.
func TestTheSeparationRuleWouldNoticeANestedLayout(t *testing.T) {
	t.Parallel()

	const nested = `
services:
  engine:
    image: backup-manager:dev
    command: ["/backup-manager-web", "serve", "--profile=generic"]
    volumes:
      - /srv/backups:/data/backups
      - /srv/backups/private:/data/state
      - /srv/backups/keys/id_ed25519:/etc/backup-manager/id_ed25519:ro
`
	doc, err := compose.Parse([]byte(nested), "synthetic-nested.yaml", separationEnv())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	backups, ok := doc.MountFor(compose.RoleEngine, backupDataPath)
	if !ok {
		t.Fatal("the synthetic document declares no backup mount, so this control proves nothing")
	}

	for _, containerPath := range []string{"/data/state", "/etc/backup-manager/id_ed25519"} {
		mount, declared := doc.MountFor(compose.RoleEngine, containerPath)
		if !declared {
			t.Fatalf("the synthetic document declares no mount at %s", containerPath)
		}
		if !compose.Contains(backups.HostPath, mount.HostPath) {
			t.Errorf("a mount at %q inside the backup destination %q was not reported as nested, so the rule above fails open",
				mount.HostPath, backups.HostPath)
		}
	}
}

// ---------------------------------------------------------------------
// Unraid: a claimed platform that is not a Compose file
// ---------------------------------------------------------------------

// unraidTemplate is the part of an Unraid Docker template this rule
// needs: the operator-editable Config rows that become bind mounts.
type unraidTemplate struct {
	Configs []unraidConfig `xml:"Config"`
}

type unraidConfig struct {
	Name    string `xml:"Name,attr"`
	Target  string `xml:"Target,attr"`
	Default string `xml:"Default,attr"`
	Type    string `xml:"Type,attr"`
	Value   string `xml:",chardata"`
}

// mounts turns the template's Path rows into the same Mount shape the
// Compose side produces, so one rule can read both. The element's own
// text is the value the operator gets; Default is the fallback for a row
// that ships empty.
func (tpl unraidTemplate) mounts() map[string]compose.Mount {
	out := map[string]compose.Mount{}
	for _, c := range tpl.Configs {
		if !strings.EqualFold(c.Type, "Path") {
			continue
		}
		host := strings.TrimSpace(c.Value)
		if host == "" {
			host = strings.TrimSpace(c.Default)
		}
		out[c.Target] = compose.Mount{HostPath: host, ContainerPath: c.Target}
	}
	return out
}

// TestTheUnraidTemplateKeepsPrivateStateOutOfTheBackupShare closes the
// biggest half of M8's platform gap.
//
// apps/unraid/template/backup-manager.xml declares exactly the mounts
// this rule is about (`/mnt/user/appdata/backup-manager/state`,
// `.../secrets/id_ed25519`, `.../secrets/known_hosts` and
// `/mnt/user/backups/backup-manager`) as operator-editable Config
// defaults, and nothing checked that an operator who repoints Backup root
// at `/mnt/user/appdata/backup-manager` has just nested the SFTP private
// key and the local-auth record inside the backup share. It was outside
// the suite entirely because the suite only ever read Compose documents.
func TestTheUnraidTemplateKeepsPrivateStateOutOfTheBackupShare(t *testing.T) {
	t.Parallel()

	const rel = "apps/unraid/template/backup-manager.xml"
	raw, err := os.ReadFile(compose.Path(rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	var tpl unraidTemplate
	if err := xml.Unmarshal(raw, &tpl); err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	mounts := tpl.mounts()
	if len(mounts) == 0 {
		t.Fatalf("%s declares no Path rows at all, so every comparison below would be about nothing", rel)
	}

	checkSeparation(t, rel, func(containerPath string) (compose.Mount, bool) {
		m, ok := mounts[containerPath]
		return m, ok
	})
}

// TestTheUnraidTemplateReaderSeesTheDeclaredPaths is the reader's own
// positive control: an XML walk that silently found nothing would make
// the test above pass on any template at all, including one that nests
// the private key inside the backup share.
func TestTheUnraidTemplateReaderSeesTheDeclaredPaths(t *testing.T) {
	t.Parallel()

	const nested = `<?xml version="1.0"?>
<Container version="2">
  <Config Name="Application state" Target="/data/state" Default="/mnt/user/backups/backup-manager/state" Type="Path">/mnt/user/backups/backup-manager/state</Config>
  <Config Name="Backup root" Target="/data/backups" Default="/mnt/user/backups/backup-manager" Type="Path">/mnt/user/backups/backup-manager</Config>
  <Config Name="Listen address" Target="LISTEN_ADDR" Default=":8080" Type="Variable">:8080</Config>
</Container>`

	var tpl unraidTemplate
	if err := xml.Unmarshal([]byte(nested), &tpl); err != nil {
		t.Fatalf("parse: %v", err)
	}
	mounts := tpl.mounts()

	if _, ok := mounts["LISTEN_ADDR"]; ok {
		t.Error("the reader turned a Variable row into a mount, so it would compare an environment value against a host path")
	}
	state, ok := mounts["/data/state"]
	if !ok {
		t.Fatal("the reader found no mount at /data/state, so the real template's rows are not being read either")
	}
	backups, ok := mounts["/data/backups"]
	if !ok {
		t.Fatal("the reader found no mount at /data/backups")
	}
	if !compose.Contains(backups.HostPath, state.HostPath) {
		t.Errorf("a state path at %q inside the backup destination %q was not reported as nested, so the rule fails open on this reader",
			state.HostPath, backups.HostPath)
	}
}

// ---------------------------------------------------------------------
// The unresolved-host-path rule's own control
// ---------------------------------------------------------------------

// TestAnUnresolvedHostPathIsRefusedRatherThanCompared pins the marker
// rule, which is what stops the TrueNAS cell passing vacuously.
//
// The failure it prevents is subtle and total: two host paths that are
// still `{{ .Values.storage.state.hostPath }}` and
// `{{ .Values.storage.backups.hostPath }}` are not equal, neither
// contains the other, and every assertion in this suite therefore passes
// on an artifact nobody has actually checked.
func TestAnUnresolvedHostPathIsRefusedRatherThanCompared(t *testing.T) {
	t.Parallel()

	for _, unresolved := range []string{
		"{{ .Values.storage.state.hostPath }}",
		"${DISK",
		"$(pwd)/state",
	} {
		if got := unresolvedIn(unresolved); len(got) == 0 {
			t.Errorf("unresolvedIn(%q) reported nothing, so this suite would compare it as though it were a host path", unresolved)
		}
	}
	// The negative half: a real path must not be flagged, or the rule
	// would fail every artifact and prove nothing about any of them.
	for _, resolved := range []string{"/mnt/tank/backup-manager/state", "/srv/backup-manager/secrets/id_ed25519"} {
		if got := unresolvedIn(resolved); len(got) != 0 {
			t.Errorf("unresolvedIn(%q) = %v, want none", resolved, got)
		}
	}

	// And the composition: the rule is actually wired into the walk. A
	// document whose host paths are unexpanded placeholders must fail,
	// not pass.
	const templated = `
services:
  engine:
    image: backup-manager:dev
    command: ["/backup-manager-web", "serve", "--profile=generic"]
    volumes:
      - "{{ .Values.storage.backups.hostPath }}:/data/backups"
      - "{{ .Values.storage.state.hostPath }}:/data/state"
`
	doc, err := compose.Parse([]byte(templated), "synthetic-templated.yaml", separationEnv())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	backups, ok := doc.MountFor(compose.RoleEngine, backupDataPath)
	if !ok {
		t.Fatal("the synthetic document declares no backup mount, so this control proves nothing")
	}
	state, ok := doc.MountFor(compose.RoleEngine, "/data/state")
	if !ok {
		t.Fatal("the synthetic document declares no state mount")
	}
	if backups.HostPath == state.HostPath || compose.Contains(backups.HostPath, state.HostPath) {
		t.Fatal("the two placeholders happen to compare as nested, so this document cannot demonstrate the silent pass")
	}
	if len(unresolvedIn(backups.HostPath)) == 0 || len(unresolvedIn(state.HostPath)) == 0 {
		t.Fatal("an unexpanded placeholder reached the comparison without being reported, which is exactly how the TrueNAS cell passed while checking nothing")
	}
}
