package apiclient

import (
	"strings"
	"testing"

	"github.com/spdrman/backupd/core/apicontract"
)

// fillPath's own rules, driven directly.
//
// Everything else in this package reaches fillPath through call(), which
// only ever hands it values a test wrote out in full, so the cases that
// matter here are the ones no ordinary call produces: a value that is
// empty, and a value that is a relative-path segment rather than a name.
// url.PathEscape leaves "." and ".." exactly as they are, because they are
// legal path characters, and a client whose whole claim is that the
// contract decides the path must not be talked into building one that
// climbs out of it.

func fillPathEndpoint(t *testing.T, id string) apicontract.Endpoint {
	t.Helper()
	ep, ok := endpointByID[id]
	if !ok {
		t.Fatalf("the contract no longer declares %q, so this test is pinning nothing", id)
	}
	return ep
}

func TestFillPath_RefusesAValueThatIsNotAName(t *testing.T) {
	ep := fillPathEndpoint(t, "getBackupSet")

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"an empty value", []string{"", "postgres"}, "empty"},
		{"an empty value in the second parameter", []string{"production", ""}, "empty"},
		// url.PathEscape returns these unchanged, so nothing downstream
		// would have escaped them either.
		{"a single dot", []string{".", "postgres"}, "relative"},
		{"a double dot", []string{"..", "postgres"}, "relative"},
		{"a double dot in the last parameter", []string{"production", ".."}, "relative"},
		{"too few values", []string{"production"}, "parameter"},
		{"too many values", []string{"production", "postgres", "extra"}, "parameter"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := fillPath(ep, tc.args)
			if err == nil {
				t.Fatalf("fillPath(%q) built %q; a path this client cannot stand behind has to be a failure rather than a best effort", tc.args, got)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("fillPath(%q) failed with %q, which does not say %q, so it does not tell a reader which rule they broke", tc.args, err, tc.want)
			}
		})
	}
}

// TestFillPath_BuildsThePathTheContractPublishes is the negative control
// for the test above: every rejection there is worthless if fillPath
// refuses ordinary values too, and two of these look like the refused ones
// without being them.
func TestFillPath_BuildsThePathTheContractPublishes(t *testing.T) {
	cases := []struct {
		name      string
		operation string
		args      []string
		want      string
	}{
		{"an ordinary two-part identity", "getBackupSet", []string{"production", "postgres"}, "/backup-sets/production/postgres"},
		{"an ordinary three-part identity", "getArtifact", []string{"production", "postgres", "2026-08-30.dump"}, "/backups/production/postgres/2026-08-30.dump"},
		// A separator inside one value is escaped rather than refused,
		// which is the whole reason the contract spells these paths one
		// parameter per segment: the escape is right, and a template that
		// made a caller rely on it spanning segments was not.
		{"a value carrying a separator", "getBackupSet", []string{"a/b", "postgres"}, "/backup-sets/a%2Fb/postgres"},
		{"a value carrying a space", "getBackupSet", []string{"a b", "postgres"}, "/backup-sets/a%20b/postgres"},
		// Dots are ordinary characters in a backup's name. Only a value
		// that is NOTHING BUT dots is a relative-path segment.
		{"a name with dots in it", "getArtifact", []string{"production", "postgres", "..2026-08-30.dump"}, "/backups/production/postgres/..2026-08-30.dump"},
		{"an operation with no parameter at all", "listBackupSets", nil, "/backup-sets"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := fillPath(fillPathEndpoint(t, tc.operation), tc.args)
			if err != nil {
				t.Fatalf("fillPath(%q): %v", tc.args, err)
			}
			if got != tc.want {
				t.Errorf("fillPath(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}
