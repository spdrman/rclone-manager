package backend

import (
	"reflect"
	"sort"
	"testing"
)

// Tests for issue #731 (EPIC I / #664): sftp as a DESTINATION backend.
//
// #665 section 4.2 named this file's subject before it existed: sftp is
// already in rclone.RequiredBackends because a backup SOURCE is read
// over it, so shipping it as a destination costs a manifest and an entry
// in SupportedRcloneBackends, with no blank import and no binary-size
// delta (see doc.go, "What is genuinely weakened"). What that paragraph
// asks for in exchange is that the decision be a reviewed diff, which is
// what the two pins next door (TestTheBundledSetIsExactly here, and
// TestEveryBundledManifestNamesABackendThisBinaryRegisters in
// core/internal/transport/rclone) make it.
//
// This file is the manifest's own half: that the file ships, that a
// consumer can find it through the accessors every consumer reads, and
// that the fields it declares are the ones an SSH destination actually
// needs - host-key verification among them, because a destination that
// can be authored without one is a destination this product would write
// backups to over an unverified connection, which is exactly what
// internal/transport/rclone/ssh.go refuses to let a SOURCE do.

// sftpInstance is a fully populated, legal sftp instance. A test breaks
// exactly one field of a copy, which is storage_mediums_test.go's own
// discipline.
func sftpInstance() map[string]string {
	return map[string]string{
		"host":                "backup.example.internal",
		"port":                "2222",
		"user":                "backupuser",
		"path":                "/srv/backups",
		"prefix":              "rclone-manager",
		"known_hosts":         "/etc/backup-manager/known_hosts",
		"upload_verification": "readback",
		"credentials":         "6f1c1f3a-0e1b-4b1e-9a1e-2f1c1f3a0e1b",
	}
}

// TestTheSftpManifestIsReachableThroughTheRegistry is the wizard's own
// read path, one layer down: core/service.RegisteredBackends walks
// IDs() and projects Backend(id) field for field, and
// apps/common/webhost serves that as GET /api/v1/backends. A manifest
// that ships but is not reachable through these two accessors is a file
// in a directory, not a destination anybody can choose.
func TestTheSftpManifestIsReachableThroughTheRegistry(t *testing.T) {
	reg, err := Bundled()
	if err != nil {
		t.Fatalf("Bundled(): %v", err)
	}

	found := false
	for _, id := range reg.IDs() {
		if id == "sftp" {
			found = true
		}
	}
	if !found {
		t.Fatalf("IDs() = %v, and the catalogue every surface reads does not offer sftp", reg.IDs())
	}

	m, err := reg.Backend("sftp")
	if err != nil {
		t.Fatalf("Backend(\"sftp\"): %v", err)
	}
	if m.Label == "" || m.Summary == "" {
		t.Errorf("the sftp manifest declares label %q and summary %q; the add-a-destination picker renders both, and an entry with neither cannot be recognised or searched for", m.Label, m.Summary)
	}
}

// TestTheSftpManifestIsRegisteredAndNotYetConfigurable is the honest
// half of #731, and the two claims have to hold together: the manifest
// is REGISTERED, so somebody searching a picker for SFTP is answered
// with the real shape rather than with silence, and it is not
// CONFIGURABLE, so no surface offers a row whose every path ends in a
// refusal.
//
// The second claim is not decoration. Nothing behind this package can
// store or dial an instance yet: config.expressibleBackendIDs reports
// [local_volume s3] because StorageMedium has no field for a host, a
// user or a known_hosts file; service's field mapper names the field it
// cannot store; internal/app's mediumType refuses RoleRemoteFilesystem.
// A manifest that claimed to be configurable while all three refused
// would be a wizard that collects eight values and then fails on save,
// which is a worse answer than the one this flag gives.
//
// The other two shipped manifests are asserted here too, because the
// default is what makes this flag safe to add: if silence meant false,
// this test would pass while the two backends an operator actually uses
// went dark.
func TestTheSftpManifestIsRegisteredAndNotYetConfigurable(t *testing.T) {
	reg, err := Bundled()
	if err != nil {
		t.Fatalf("Bundled(): %v", err)
	}

	m, err := reg.Backend("sftp")
	if err != nil {
		t.Fatalf("Backend(\"sftp\"): %v", err)
	}
	if m.IsConfigurable() {
		t.Error("the sftp manifest reports itself configurable, and nothing in this build can store or dial an instance of it: a picker reading this offers a row that cannot be saved")
	}

	for _, id := range []string{"local_volume", "s3"} {
		other, err := reg.Backend(id)
		if err != nil {
			t.Fatalf("Backend(%q): %v", id, err)
		}
		if !other.IsConfigurable() {
			t.Errorf("the %s manifest reports itself not configurable; it is a destination an operator can author today, and a picker reading this would refuse to offer it", id)
		}
	}
}

// TestTheSftpManifestDeclaresAnSSHDestination is what the manifest IS,
// asserted against the facts a consumer depends on rather than against
// the file's text.
func TestTheSftpManifestDeclaresAnSSHDestination(t *testing.T) {
	reg, err := Bundled()
	if err != nil {
		t.Fatalf("Bundled(): %v", err)
	}
	m, err := reg.Backend("sftp")
	if err != nil {
		t.Fatalf("Backend(\"sftp\"): %v", err)
	}

	if m.Role != RoleRemoteFilesystem {
		t.Errorf("the sftp manifest declares role %q; a directory on another host is neither an object store nor a volume this machine can see, and internal/app's mediumType dispatches on exactly this", m.Role)
	}
	if m.RcloneBackend != "sftp" {
		t.Errorf("the sftp manifest declares rclone_backend %q, want \"sftp\"", m.RcloneBackend)
	}
	if !SupportedRcloneBackends["sftp"] {
		t.Error("SupportedRcloneBackends does not name \"sftp\", so the manifest above could not have loaded and this build cannot dial its own destination")
	}

	// The required set is the load-bearing half. known_hosts is on it
	// because FR-6 makes host-key verification mandatory for sftp
	// (core/internal/config/validate.go refuses a source without one,
	// and ssh.go refuses rclone's own "accept any key" default), and an
	// optional known_hosts would be a destination an operator can
	// author without one.
	var required, optional []string
	for _, f := range m.Fields {
		if f.Required {
			required = append(required, f.ID)
		} else {
			optional = append(optional, f.ID)
		}
	}
	sort.Strings(required)
	sort.Strings(optional)
	wantRequired := []string{"credentials", "host", "known_hosts", "path", "user"}
	wantOptional := []string{"port", "prefix", "upload_verification"}
	if !reflect.DeepEqual(required, wantRequired) {
		t.Errorf("the sftp manifest's required fields are %v, want %v", required, wantRequired)
	}
	if !reflect.DeepEqual(optional, wantOptional) {
		t.Errorf("the sftp manifest's optional fields are %v, want %v", optional, wantOptional)
	}

	cred, ok := m.CredentialField()
	if !ok {
		t.Fatal("the sftp manifest declares no credential field; an SSH destination authenticates with a key it has to be given")
	}
	if cred.ID != "credentials" {
		t.Errorf("the sftp manifest's credential field is %q; \"credentials\" is the id core/cliecho's wire spellings and core/service's credential reference are built for", cred.ID)
	}

	// TODO(#235): `path` is a directory on the FAR host and `known_hosts`
	// is a file on THIS one, and both are KindPath, whose rules are this
	// machine's. Deferred with the manifest not configurable: the
	// distinction is unreachable until something dials an instance, and
	// #235 is where a remote-path kind is decided rather than guessed at
	// here.
	if path, _ := m.Field("path"); path.Kind != KindPath {
		t.Errorf("the sftp manifest's path field is kind %q, want %q: the directory on the far host is a path, and KindPath is what refuses a relative or unclean one", path.Kind, KindPath)
	}
	if prefix, _ := m.Field("prefix"); prefix.Kind != KindKeyPrefix {
		t.Errorf("the sftp manifest's prefix field is kind %q, want %q", prefix.Kind, KindKeyPrefix)
	}

	// TODO(#235): the host pattern refuses whitespace and a separator and
	// nothing else, so it accepts strings no resolver ever will. Deferred
	// with the manifest not configurable: a real host rule belongs next
	// to the code that dials one, which #235 adds.
	if host, _ := m.Field("host"); host.Pattern == "" {
		t.Error("the sftp manifest's host field declares no pattern; a URL pasted into that box would be stored as a hostname")
	}
}

// TestTheSftpProbeRunsEveryCheckAStorelessDestinationCan pins which of
// mediumcheck's steps an sftp destination declares, and specifically
// that the one it skips is the one it genuinely cannot do. The shape
// rules (every step, once, in order, a skip carries a reason) are
// validateManifestProbe's and are tested there; what is asserted here is
// the ANSWER this manifest gives, because a destination that quietly
// skipped write or read_back would report a working connection having
// proven nothing.
func TestTheSftpProbeRunsEveryCheckAStorelessDestinationCan(t *testing.T) {
	reg, err := Bundled()
	if err != nil {
		t.Fatalf("Bundled(): %v", err)
	}
	m, err := reg.Backend("sftp")
	if err != nil {
		t.Fatalf("Backend(\"sftp\"): %v", err)
	}

	run := map[string]bool{}
	reason := map[string]string{}
	for _, s := range m.Probe.Steps {
		run[s.Step] = s.Run
		reason[s.Step] = s.Reason
	}
	for _, step := range []string{"credentials", "reach", "deliverable", "write", "read_back", "verification", "delete"} {
		if !run[step] {
			t.Errorf("the sftp probe skips %q, and an SSH destination can do it", step)
		}
	}
	if run["storage_class"] {
		t.Error("the sftp probe claims to check storage_class, and a directory on an SSH server has no storage classes to check")
	}
	if reason["storage_class"] == "" {
		t.Error("the sftp probe skips storage_class with no reason; a surface has to be able to tell an operator why")
	}
}

// TestAnSftpInstanceIsJudgedByTheManifestsOwnRules drives
// ValidateInstance against the shapes this manifest declares, which is
// the only place the host, port and user patterns can be proven to mean
// anything: a pattern that compiles and matches everything is a pattern
// that passes every other test in this package.
func TestAnSftpInstanceIsJudgedByTheManifestsOwnRules(t *testing.T) {
	reg, err := Bundled()
	if err != nil {
		t.Fatalf("Bundled(): %v", err)
	}

	if errs := reg.ValidateInstance("sftp", "storage_mediums[0]", sftpInstance()); len(errs) != 0 {
		t.Fatalf("a fully populated sftp instance was refused: %v", errs)
	}

	for _, tc := range []struct {
		name  string
		field string
		value string
	}{
		{"no host", "host", ""},
		{"a host carrying a path", "host", "backup.example.internal/srv"},
		{"a host carrying a scheme", "host", "sftp://backup.example.internal"},
		{"no user", "user", ""},
		{"a user carrying a separator", "user", "backup/user"},
		{"a port that is not a number", "port", "twenty-two"},
		{"a port above the range", "port", "70000"},
		{"port zero", "port", "0"},
		{"no path", "path", ""},
		{"a relative path", "path", "srv/backups"},
		{"a path with a traversal segment", "path", "/srv/../etc"},
		{"no known_hosts", "known_hosts", ""},
		{"a relative known_hosts", "known_hosts", "known_hosts"},
		{"an unknown verification mode", "upload_verification", "trust_me"},
		{"no credential", "credentials", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := sftpInstance()
			values[tc.field] = tc.value
			if errs := reg.ValidateInstance("sftp", "storage_mediums[0]", values); len(errs) == 0 {
				t.Fatalf("an sftp instance with %s was accepted", tc.name)
			}
		})
	}

	t.Run("an empty port is the SSH default", func(t *testing.T) {
		// Port is optional and there is no unset_means for a string
		// field to carry (only an enum may), so "empty means 22" lives
		// in the field's help where an operator reads it, exactly as
		// config.Remote.Port's own "0 means the backend's default port"
		// does. What matters here is that leaving it empty is legal.
		values := sftpInstance()
		values["port"] = ""
		if errs := reg.ValidateInstance("sftp", "storage_mediums[0]", values); len(errs) != 0 {
			t.Fatalf("an sftp instance with no port was refused: %v", errs)
		}
	})
}
