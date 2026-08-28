package model

import "testing"

func TestBackupSetIDRoundTrips(t *testing.T) {
	id, err := NewBackupSetID("production", "postgres-primary")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	if got, want := id.String(), "production/postgres-primary"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	back, err := ParseBackupSetID(id.String())
	if err != nil {
		t.Fatalf("ParseBackupSetID: %v", err)
	}
	if back != id {
		t.Fatalf("round trip changed the id: %#v vs %#v", back, id)
	}
}

// The isolation property FR-7 actually rests on: two different sets must never
// collapse to the same identity, because retention keyed on a colliding id
// would let one set's pass delete another set's restore points.
func TestDifferentSetsNeverCollide(t *testing.T) {
	seen := map[string]BackupSetID{}
	for _, pair := range [][2]string{
		{"production", "postgres-primary"},
		{"production", "uploads"},
		{"staging", "postgres-primary"},
		{"staging", "uploads"},
	} {
		id, err := NewBackupSetID(pair[0], pair[1])
		if err != nil {
			t.Fatalf("NewBackupSetID(%q,%q): %v", pair[0], pair[1], err)
		}
		if prev, dup := seen[id.String()]; dup {
			t.Fatalf("%#v and %#v both render as %q", prev, id, id.String())
		}
		seen[id.String()] = id
	}
	if len(seen) != 4 {
		t.Fatalf("expected 4 distinct ids, got %d", len(seen))
	}
}

// The ambiguity the separator ban exists to prevent. Without it
// {"a", "b/c"} and {"a/b", "c"} would both render as "a/b/c".
func TestSeparatorInEitherHalfIsRefused(t *testing.T) {
	if _, err := NewBackupSetID("a", "b/c"); err == nil {
		t.Fatal("a set containing a slash was accepted, so ids are ambiguous")
	}
	if _, err := NewBackupSetID("a/b", "c"); err == nil {
		t.Fatal("a source containing a slash was accepted, so ids are ambiguous")
	}
}

func TestBackupSetIDRejectsJunk(t *testing.T) {
	for _, tc := range []struct{ name, source, set string }{
		{"empty source", "", "s"},
		{"empty set", "p", ""},
		{"padded source", " p", "s"},
		{"padded set", "p", "s "},
		{"newline", "p", "s\n"},
		{"nul", "p", "s\x00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewBackupSetID(tc.source, tc.set); err == nil {
				t.Fatalf("accepted source=%q set=%q", tc.source, tc.set)
			}
		})
	}
}

func TestArtifactNameMustBeABasename(t *testing.T) {
	set, err := NewBackupSetID("production", "postgres-primary")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	if _, err := NewArtifactID(set, "backup-2026-08-27.dump.zst"); err != nil {
		t.Fatalf("a plain basename was refused: %v", err)
	}
	// Remote metadata is untrusted (FR-8). A traversal name must not survive
	// long enough to reach a delete.
	for _, bad := range []string{"", ".", "..", "../../etc/passwd", "sub/nested.dump", "a\\b", "x\x00y", " padded"} {
		if _, err := NewArtifactID(set, bad); err == nil {
			t.Fatalf("accepted hostile artifact name %q", bad)
		}
	}
}

func TestZeroSetIsRefusedForArtifacts(t *testing.T) {
	if _, err := NewArtifactID(BackupSetID{}, "x.dump"); err == nil {
		t.Fatal("a zero backup set was accepted, which would make the artifact span every set")
	}
}
