package backupengine_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/backupdproject/backupd/core/internal/backupengine"
)

// TestReservedLocalDirRefusesWhatWouldEscapeIt covers the inputs that
// would turn a reserved namespace into something else.
//
// A relative root is the dangerous one and the reason it is refused rather
// than resolved: the namespace would be reserved relative to whatever
// directory the process happened to be in, so the predicate artifact
// management consults and the directory the engine writes into could
// disagree without either being wrong.
func TestReservedLocalDirRefusesWhatWouldEscapeIt(t *testing.T) {
	t.Parallel()

	domain := domainID(t, "production")

	for _, tc := range []struct {
		name   string
		root   string
		domain string
		says   string
	}{
		{"no root", "", "production", "backup root"},
		{"a relative root", "srv/backups", "production", "relative"},
		{"no domain", "/srv/backups", "", "repository domain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var d = domain
			if tc.domain == "" {
				d = ""
			}

			_, err := backupengine.ReservedLocalDir(tc.root, d)
			if err == nil {
				t.Fatalf("ReservedLocalDir(%q, %q) was accepted", tc.root, tc.domain)
			}

			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("the refusal does not say %q: %v", tc.says, err)
			}
		})
	}
}

// TestReservedLocalDirIsUniquePerRepository is what stops two repository
// domains from sharing one directory, which would have them overwriting
// each other's format blob.
func TestReservedLocalDirIsUniquePerRepository(t *testing.T) {
	t.Parallel()

	first, err := backupengine.ReservedLocalDir("/srv/backups", domainID(t, "production"))
	if err != nil {
		t.Fatalf("ReservedLocalDir: %v", err)
	}

	second, err := backupengine.ReservedLocalDir("/srv/backups", domainID(t, "customer-a"))
	if err != nil {
		t.Fatalf("ReservedLocalDir: %v", err)
	}

	if first == second {
		t.Fatalf("two repository domains resolve to the same directory %s", first)
	}

	// And both are inside the one namespace artifact management stays out
	// of, which is the property that makes one predicate enough.
	for _, dir := range []string{first, second} {
		if !backupengine.LocalPathIsReserved("/srv/backups", dir) {
			t.Errorf("%s is not inside the reserved namespace of /srv/backups", dir)
		}
	}
}

// TestLocalPathIsReservedComparesPathsNotStrings is the boundary case a
// prefix comparison gets wrong in both directions, which is why the
// implementation uses filepath.Rel rather than strings.HasPrefix.
func TestLocalPathIsReservedComparesPathsNotStrings(t *testing.T) {
	t.Parallel()

	const root = "/srv/backups"

	reserved, err := backupengine.ReservedLocalDir(root, domainID(t, "production"))
	if err != nil {
		t.Fatalf("ReservedLocalDir: %v", err)
	}

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"a repository blob", filepath.Join(reserved, "p", "a1b", "c2d3"), true},
		{"the reserved directory itself", filepath.Join(root, ".backupd"), true},
		{"local state beside the repositories", filepath.Join(root, ".backupd", "state", "production.config"), true},
		{"an unclean path into the namespace", filepath.Join(root, ".", ".backupd", "x"), true},
		{"an artifact", filepath.Join(root, "production", "pg", "dump.tar.gz"), false},
		{"a directory whose name merely starts the same", filepath.Join(root, ".backupdata", "x"), false},
		{"a sibling of the backup root", "/srv/other/.backupd/x", false},
		{"the backup root itself", root, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := backupengine.LocalPathIsReserved(root, tc.path); got != tc.want {
				t.Errorf("LocalPathIsReserved(%q, %q) = %v, want %v", root, tc.path, got, tc.want)
			}
		})
	}
}
