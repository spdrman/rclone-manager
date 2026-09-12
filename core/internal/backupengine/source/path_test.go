package source_test

import (
	"errors"
	"path"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/backupdproject/backupd/core/internal/backupengine/source"
)

// The named attacks, one row each, because a property test proves the
// invariant and a table proves that the specific things somebody will try
// are actually among the inputs the invariant covers.
func TestSafeRelPathRefusesEveryWayOutOfTheRoot(t *testing.T) {
	t.Parallel()

	refused := []struct {
		name string
		in   string
	}{
		{"traversal", "../etc/shadow"},
		{"traversal in the middle", "runs/../../etc/shadow"},
		{"traversal that cleans back inside", "runs/2026/../../../../etc/shadow"},
		{"bare parent", ".."},
		{"absolute", "/etc/shadow"},
		{"absolute with traversal", "/../etc/shadow"},
		{"nul byte", "safe.txt\x00/../../etc/shadow"},
		{"nul terminator", "safe.txt\x00"},
		{"windows drive", `C:\Windows\System32\config\SAM`},
		{"windows drive relative", "C:Windows"},
		{"windows drive forward slashes", "C:/Windows"},
		{"unc", `\\server\share\file`},
		{"backslash separator", `runs\2026\db.dump`},
		{"backslash in a name", `odd\name.txt`},
		{"empty", ""},
		{"dot", "."},
		{"dot slash dot", "./."},
		{"invalid utf8", "runs/\xff\xfe.dump"},
		{"overlong", strings.Repeat("a/", 4096)},
	}

	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := source.SafeRelPath(tc.in)
			if err == nil {
				t.Fatalf("SafeRelPath(%q) = %q, want a refusal", tc.in, got)
			}
			if !errors.Is(err, source.ErrUnsafePath) {
				t.Fatalf("SafeRelPath(%q) refused with %v, which does not wrap ErrUnsafePath, so no caller can branch on it", tc.in, err)
			}
		})
	}
}

// The spellings that are cosmetic rather than dangerous are cleaned, not
// refused: refusing them would drop real files out of a backup because
// somebody's producer emitted a trailing slash.
func TestSafeRelPathCanonicalisesHarmlessSpellings(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]string{
		"runs/2026/db.dump":      "runs/2026/db.dump",
		"runs//2026//db.dump":    "runs/2026/db.dump",
		"./runs/2026/db.dump":    "runs/2026/db.dump",
		"runs/./2026/db.dump":    "runs/2026/db.dump",
		"runs/2026/":             "runs/2026",
		"a b/c d.txt":            "a b/c d.txt",
		"Ünïcøde/naïve.txt":      "Ünïcøde/naïve.txt",
		"runs/2026/..hidden":     "runs/2026/..hidden",
		"runs/2026/...deceptive": "runs/2026/...deceptive",
	} {
		got, err := source.SafeRelPath(in)
		if err != nil {
			t.Errorf("SafeRelPath(%q): %v", in, err)

			continue
		}
		if got != want {
			t.Errorf("SafeRelPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// The property, over arbitrary bytes: whatever SafeRelPath accepts is
// inside the root, stays inside the root when joined onto one, and is
// already canonical so a second pass cannot move it.
//
// The last clause is the one that catches a subtle class of bug: a
// sanitiser whose output is not a fixed point is a sanitiser whose
// callers get different answers depending on how many times the value has
// been through it.
func FuzzSafeRelPathCannotEscapeTheRoot(f *testing.F) {
	for _, seed := range []string{
		"runs/2026/db.dump", "../etc/shadow", "/etc/shadow", "..", ".",
		"a/../../b", "C:/x", `x\y`, "x\x00y", "\xff", "", "///",
		"a/./b", "a//b/", "....//....//etc", "AAA/../..",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, in string) {
		got, err := source.SafeRelPath(in)
		if err != nil {
			return
		}

		if got == "" || got == "." || got == ".." {
			t.Fatalf("SafeRelPath(%q) accepted %q, which names no object", in, got)
		}

		if strings.HasPrefix(got, "/") {
			t.Fatalf("SafeRelPath(%q) accepted the absolute path %q", in, got)
		}

		if strings.HasPrefix(got, "../") || strings.Contains(got, "/../") || strings.HasSuffix(got, "/..") {
			t.Fatalf("SafeRelPath(%q) accepted %q, which walks out of the root", in, got)
		}

		if strings.ContainsRune(got, 0) || strings.Contains(got, `\`) {
			t.Fatalf("SafeRelPath(%q) accepted %q, which is not an unambiguous name", in, got)
		}

		if !utf8.ValidString(got) {
			t.Fatalf("SafeRelPath(%q) accepted %q, which is not valid UTF-8", in, got)
		}

		// Joined onto any root, the result must still be under it. This
		// is the escape property stated the way a caller experiences it.
		for _, root := range []string{"/srv/backup", "/", "relative/root"} {
			joined := path.Clean(root + "/" + got)
			if !strings.HasPrefix(joined, path.Clean(root)) {
				t.Fatalf("SafeRelPath(%q) = %q, which joined onto %q gives %q, outside it", in, got, root, joined)
			}
		}

		// Idempotence: the accepted form is a fixed point.
		again, err := source.SafeRelPath(got)
		if err != nil {
			t.Fatalf("SafeRelPath(%q) = %q, which SafeRelPath then refused: %v", in, got, err)
		}
		if again != got {
			t.Fatalf("SafeRelPath is not idempotent: %q -> %q -> %q", in, got, again)
		}
	})
}

// Case variation must not change the ANSWER, which is the property that
// makes fold-blindness safe here: a denylist would be bypassable by case,
// and a structural check is not. Proven over the traversal spellings
// rather than asserted in a comment.
func TestSafeRelPathIsCaseIndependent(t *testing.T) {
	t.Parallel()

	for _, in := range []string{
		"../ETC/Shadow", "..%2F", "RUNS/../../etc", "c:/windows", "C:/WINDOWS",
		"Runs/2026/DB.dump",
	} {
		lower, lowerErr := source.SafeRelPath(strings.ToLower(in))
		upper, upperErr := source.SafeRelPath(strings.ToUpper(in))

		if (lowerErr == nil) != (upperErr == nil) {
			t.Errorf("%q is refused in one case and accepted in the other: lower=%v upper=%v", in, lowerErr, upperErr)
		}

		if lowerErr == nil && !strings.EqualFold(lower, upper) {
			t.Errorf("%q resolves to %q lowercased and %q uppercased, which are not the same name", in, lower, upper)
		}
	}
}

// Contains is what the symlink-target check rests on, so its boundary
// cases are checked directly: a prefix that is not a path boundary is not
// containment, which is the classic "/srv/backup-evil is under
// /srv/backup" bug.
func TestContainsIsAPathBoundaryAndNotAStringPrefix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		root, child string
		want        bool
	}{
		{"runs", "runs", true},
		{"runs", "runs/2026", true},
		{"runs", "runs-evil", false},
		{"runs", "runsevil/x", false},
		{"runs/2026", "runs", false},
		{"", "anything", true},
		{"runs", "other/runs", false},
	}

	for _, tc := range cases {
		if got := source.Contains(tc.root, tc.child); got != tc.want {
			t.Errorf("Contains(%q, %q) = %v, want %v", tc.root, tc.child, got, tc.want)
		}
	}
}
