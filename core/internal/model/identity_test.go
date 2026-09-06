// CompareIdentity, checked against the situations it exists for rather than
// against its own branches.
//
// The table is built by mutating one attribute of a shared base identity,
// so every case reads as "the same object except this", which is exactly
// the question FR-16 asks before a delete. Cases where a signal is missing
// entirely matter as much as cases where it disagrees: the whole design
// rests on degrading honestly when a backend cannot hash, and a suite that
// only ever supplied hashes would never exercise the ladder below them.
//
// The awkward case is the one to keep: same path, same size, same
// modification time, different content. That is what a replacement written
// inside one mtime tick looks like, it is the only reason any of this is
// more complicated than comparing two structs, and it has a positive
// control of its own beside it to prove the fixture still is that case.

package model

import "testing"

// base is a stable starting point every test case tweaks one attribute of,
// so each case's diff from the others is exactly the attribute under test.
var base = RemoteIdentity{Path: "backup.tar", Size: 1024, ModTime: 1_700_000_000}

// withHash and withStableID copy base and set one attribute, so a table row
// stays a single line and its difference from the others is the whole of
// what it is testing. They take and return values rather than pointers
// precisely so no case can mutate the shared base out from under the ones
// after it.
func withHash(r RemoteIdentity, alg, hash string) RemoteIdentity {
	r.HashAlg, r.Hash = alg, hash
	return r
}

func withStableID(r RemoteIdentity, id string) RemoteIdentity {
	r.StableID = id
	return r
}

func TestCompareIdentity(t *testing.T) {
	tests := []struct {
		name           string
		discovered     RemoteIdentity
		current        RemoteIdentity
		wantVerdict    Verdict
		wantConfidence Confidence
	}{
		{
			name:           "identical, hash agrees",
			discovered:     withHash(base, "sha256", "hash-a"),
			current:        withHash(base, "sha256", "hash-a"),
			wantVerdict:    VerdictUnchanged,
			wantConfidence: ConfidenceStrong,
		},
		{
			name:           "different path",
			discovered:     base,
			current:        func() RemoteIdentity { r := base; r.Path = "other.tar"; return r }(),
			wantVerdict:    VerdictChanged,
			wantConfidence: ConfidenceStrong,
		},
		{
			name:           "different size, no hash on either side",
			discovered:     base,
			current:        func() RemoteIdentity { r := base; r.Size = 2048; return r }(),
			wantVerdict:    VerdictChanged,
			wantConfidence: ConfidenceStrong,
		},
		{
			name:           "different modtime, no hash on either side",
			discovered:     base,
			current:        func() RemoteIdentity { r := base; r.ModTime = base.ModTime + 1; return r }(),
			wantVerdict:    VerdictChanged,
			wantConfidence: ConfidenceStrong,
		},
		{
			// The FR-16 core proof: a replacement that shares path, size AND
			// modtime with the original, and only the content (hash)
			// differs. A comparison that stopped at size/modtime agreement
			// would wrongly call this unchanged.
			name:           "awkward case: same path, same size, same modtime, different content",
			discovered:     withHash(base, "sha256", "hash-of-original-content"),
			current:        withHash(base, "sha256", "hash-of-replacement-content"),
			wantVerdict:    VerdictChanged,
			wantConfidence: ConfidenceStrong,
		},
		{
			// This is the shell-less SFTP account case: rclone's sftp
			// backend cannot compute a remote hash without running a shell
			// command on the server, so a hardened, shell-less account never
			// produces one (verified directly against a Docker sshd fixture
			// configured with ForceCommand internal-sftp and a nologin
			// shell: RemoteHash returns an explicit error, never a hash).
			// With no hash and no backend stable identifier on either side,
			// a size+modtime match is the best evidence available, and it
			// is deliberately not enough to confirm anything: mtime
			// granularity (commonly one second) cannot rule out a
			// same-second content replacement.
			name:           "no hash available on either side (shell-less SFTP), size and modtime agree",
			discovered:     base,
			current:        base,
			wantVerdict:    VerdictUnconfirmed,
			wantConfidence: ConfidenceWeak,
		},
		{
			name: "no hash, no modtime on either side, size agrees",
			discovered: RemoteIdentity{
				Path: base.Path, Size: base.Size, ModTime: 0,
			},
			current: RemoteIdentity{
				Path: base.Path, Size: base.Size, ModTime: 0,
			},
			wantVerdict:    VerdictUnconfirmed,
			wantConfidence: ConfidenceNone,
		},
		{
			name: "modtime unavailable on current side only, size agrees, no hash",
			discovered: RemoteIdentity{
				Path: base.Path, Size: base.Size, ModTime: base.ModTime,
			},
			current: RemoteIdentity{
				Path: base.Path, Size: base.Size, ModTime: 0,
			},
			wantVerdict:    VerdictUnconfirmed,
			wantConfidence: ConfidenceNone,
		},
		{
			name:           "hash on discovered side only falls back to size/modtime, which is only weak evidence",
			discovered:     withHash(base, "sha256", "hash-a"),
			current:        base, // no hash available now
			wantVerdict:    VerdictUnconfirmed,
			wantConfidence: ConfidenceWeak,
		},
		{
			name:           "hash present on both sides but algorithms differ, falls back to weak size/modtime evidence",
			discovered:     withHash(base, "sha256", "hash-a"),
			current:        withHash(base, "md5", "hash-b"),
			wantVerdict:    VerdictUnconfirmed,
			wantConfidence: ConfidenceWeak,
		},
		{
			name:           "stable id mismatch is decisive even without a hash",
			discovered:     withStableID(base, "obj-1"),
			current:        withStableID(base, "obj-2"),
			wantVerdict:    VerdictChanged,
			wantConfidence: ConfidenceStrong,
		},
		{
			name:           "stable id agreement is strong evidence even without a hash",
			discovered:     withStableID(base, "obj-1"),
			current:        withStableID(base, "obj-1"),
			wantVerdict:    VerdictUnchanged,
			wantConfidence: ConfidenceStrong,
		},
		{
			// Stable id disagreement must win even when a stale/matching
			// hash is also present, since the hash comparison runs first in
			// the priority order and would otherwise mask this.
			name: "hash agrees but stable id disagrees: hash settles it first",
			discovered: withStableID(
				withHash(base, "sha256", "hash-a"), "obj-1",
			),
			current: withStableID(
				withHash(base, "sha256", "hash-a"), "obj-2",
			),
			wantVerdict:    VerdictUnchanged,
			wantConfidence: ConfidenceStrong,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := CompareIdentity(tc.discovered, tc.current)
			if got.Verdict != tc.wantVerdict || got.Confidence != tc.wantConfidence {
				t.Fatalf("CompareIdentity() = (verdict=%v, confidence=%v, reason=%q), want (verdict=%v, confidence=%v)",
					got.Verdict, got.Confidence, got.Reason, tc.wantVerdict, tc.wantConfidence)
			}
			if got.Reason == "" {
				t.Errorf("CompareIdentity() returned an empty Reason; every verdict should explain itself for an audit log")
			}
		})
	}
}

// TestCompareIdentity_AwkwardCasePositiveControl is a standalone positive
// control for the awkward-case table entry above: it proves the fixture
// really does differ (the discovered and replacement hashes are not equal,
// and size/modtime truly do agree), so "changed" cannot be passing for the
// wrong reason such as a fixture bug that made the two sides trivially
// different some other way.
func TestCompareIdentity_AwkwardCasePositiveControl(t *testing.T) {
	discovered := withHash(base, "sha256", "hash-of-original-content")
	current := withHash(base, "sha256", "hash-of-replacement-content")

	if discovered.Path != current.Path {
		t.Fatalf("fixture bug: paths differ (%q vs %q), this is no longer the awkward case", discovered.Path, current.Path)
	}
	if discovered.Size != current.Size {
		t.Fatalf("fixture bug: sizes differ (%d vs %d), this is no longer the awkward case", discovered.Size, current.Size)
	}
	if discovered.ModTime != current.ModTime {
		t.Fatalf("fixture bug: modtimes differ (%d vs %d), this is no longer the awkward case", discovered.ModTime, current.ModTime)
	}
	if discovered.Hash == current.Hash {
		t.Fatalf("fixture bug: discovered and replacement hashed identically, the fixture never actually changed the content")
	}

	got := CompareIdentity(discovered, current)
	if got.Verdict != VerdictChanged || got.Confidence != ConfidenceStrong {
		t.Fatalf("CompareIdentity() = (verdict=%v, confidence=%v), want (changed, strong) now that the positive control confirmed same path/size/modtime and different hash",
			got.Verdict, got.Confidence)
	}
}

// TestIdentityComparison_Preserve is the FR-16 policy proof: only a
// VerdictUnchanged backed by ConfidenceStrong clears the bar to proceed with
// a pending deletion. Every other combination, including the ones that never
// occur in practice, must preserve, because the rule this exists to enforce
// is "when in doubt, keep the object".
func TestIdentityComparison_Preserve(t *testing.T) {
	tests := []struct {
		verdict    Verdict
		confidence Confidence
		want       bool
	}{
		{VerdictUnchanged, ConfidenceStrong, false},
		{VerdictUnchanged, ConfidenceWeak, true},
		{VerdictUnchanged, ConfidenceNone, true},
		{VerdictChanged, ConfidenceStrong, true},
		{VerdictUnconfirmed, ConfidenceWeak, true},
		{VerdictUnconfirmed, ConfidenceNone, true},
	}
	for _, tc := range tests {
		c := IdentityComparison{Verdict: tc.verdict, Confidence: tc.confidence}
		if got := c.Preserve(); got != tc.want {
			t.Errorf("IdentityComparison{%v, %v}.Preserve() = %v, want %v", tc.verdict, tc.confidence, got, tc.want)
		}
	}
}

// The two String methods are tested for one case each that is not a
// legitimate value: Confidence(99) and Verdict(99).
//
// That is the case worth pinning. Both types have a meaningful-looking zero
// value, "none" and "unconfirmed", so a switch that fell through to a
// default of that instead of printing the number would render an unhandled
// value as the cautious answer, inside the audit line an operator reads to
// find out why a deletion was refused.
func TestConfidenceString(t *testing.T) {
	tests := map[Confidence]string{
		ConfidenceNone:   "none",
		ConfidenceWeak:   "weak",
		ConfidenceStrong: "strong",
		Confidence(99):   "Confidence(99)",
	}
	for c, want := range tests {
		if got := c.String(); got != want {
			t.Errorf("Confidence(%d).String() = %q, want %q", int(c), got, want)
		}
	}
}

func TestVerdictString(t *testing.T) {
	tests := map[Verdict]string{
		VerdictUnconfirmed: "unconfirmed",
		VerdictUnchanged:   "unchanged",
		VerdictChanged:     "changed",
		Verdict(99):        "Verdict(99)",
	}
	for v, want := range tests {
		if got := v.String(); got != want {
			t.Errorf("Verdict(%d).String() = %q, want %q", int(v), got, want)
		}
	}
}
