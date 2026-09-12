package secretref_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/backupdproject/backupd/core/internal/config"
	"github.com/backupdproject/backupd/core/internal/secretref"
	"github.com/backupdproject/backupd/core/internal/transport"
)

// theSecret is the material every test in this file resolves. It is
// deliberately one distinctive token so a leak assertion can look for it in
// a rendered error and find it if it is there.
const theSecret = "canary-repository-passphrase-8f2a"

func writeFile(t *testing.T, name, contents string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}

	return path
}

// TestRefFieldSetMatchesTheConfiguredOnes is the anti-drift pin, and it is
// the whole argument that this package is not a second credential
// vocabulary.
//
// An operator declares a secret in exactly one shape in config.yaml
// (file/env/command). config.MediumCredentials, config.Passphrase and
// transport.MediumCredentials all say it; this package resolves it. A
// fourth field appearing on one of them and not the others would be a
// source an operator can write and this resolver silently ignores, which
// is the failure mode that makes a "no parallel secret store" claim stop
// being true.
func TestRefFieldSetMatchesTheConfiguredOnes(t *testing.T) {
	t.Parallel()

	want := fieldShape(reflect.TypeFor[secretref.Ref]())

	for _, other := range []struct {
		name string
		typ  reflect.Type
	}{
		{"transport.MediumCredentials", reflect.TypeFor[transport.MediumCredentials]()},
		{"config.MediumCredentials", reflect.TypeFor[config.MediumCredentials]()},
		{"config.Passphrase", reflect.TypeFor[config.Passphrase]()},
	} {
		if got := fieldShape(other.typ); got != want {
			t.Errorf("%s has fields %s; secretref.Ref has %s. These are the same declaration in two places, "+
				"so a field on one and not the other is a credential source an operator can write and this package cannot resolve",
				other.name, got, want)
		}
	}
}

// fieldShape renders a struct's exported field names and kinds, which is
// what must match: the yaml tags legitimately differ between the schema
// types and this one, and the doc comments certainly do.
func fieldShape(t reflect.Type) string {
	var parts []string

	for i := range t.NumField() {
		f := t.Field(i)
		parts = append(parts, f.Name+" "+f.Type.String())
	}

	return "{" + strings.Join(parts, "; ") + "}"
}

// TestRefValidateDemandsExactlyOneSource covers the two ways a reference
// can be unusable, and they are different operator mistakes: silence means
// nothing was declared, and two sources mean two things were and this
// package will not choose between them.
func TestRefValidateDemandsExactlyOneSource(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		ref  secretref.Ref
		want error
	}{
		{"none", secretref.Ref{}, secretref.ErrNoSource},
		{"file and env", secretref.Ref{File: "/tmp/x", Env: "Y"}, secretref.ErrAmbiguousSource},
		{"file and command", secretref.Ref{File: "/tmp/x", Command: []string{"/bin/true"}}, secretref.ErrAmbiguousSource},
		{"env and command", secretref.Ref{Env: "Y", Command: []string{"/bin/true"}}, secretref.ErrAmbiguousSource},
		{"all three", secretref.Ref{File: "/tmp/x", Env: "Y", Command: []string{"/bin/true"}}, secretref.ErrAmbiguousSource},
		{"file", secretref.Ref{File: "/tmp/x"}, nil},
		{"env", secretref.Ref{Env: "Y"}, nil},
		{"command", secretref.Ref{Command: []string{"/bin/true"}}, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if err := tc.ref.Validate(); !errors.Is(err, tc.want) {
				t.Errorf("Validate() = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestResolveFromEveryDeclaredSource is the round trip: whichever of the
// three an operator wrote, the same material comes back.
func TestResolveFromEveryDeclaredSource(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("file", func(t *testing.T) {
		t.Parallel()

		// The trailing newline is what every editor and every `echo >`
		// leaves behind, and a passphrase with an invisible newline on the
		// end is a repository nobody can open.
		ref := secretref.Ref{File: writeFile(t, "passphrase", theSecret+"\n")}

		got, err := secretref.Resolve(ctx, ref)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}

		if got.Reveal() != theSecret {
			t.Errorf("resolved %d bytes, want the %d-byte secret with its trailing newline removed", len(got.Reveal()), len(theSecret))
		}
	})

	t.Run("command", func(t *testing.T) {
		t.Parallel()

		got, err := secretref.Resolve(ctx, secretref.Ref{Command: []string{"/bin/echo", theSecret}})
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}

		if got.Reveal() != theSecret {
			t.Errorf("resolved the wrong material from the command")
		}
	})
}

// TestResolveFromTheEnvironment is separate and not parallel because
// t.Setenv cannot be used from a parallel test: the environment is
// process-wide state and the testing package refuses to let two tests
// disagree about it.
func TestResolveFromTheEnvironment(t *testing.T) {
	t.Setenv("BACKUPD_TEST_SECRET", theSecret+"\n")

	got, err := secretref.Resolve(context.Background(), secretref.Ref{Env: "BACKUPD_TEST_SECRET"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if got.Reveal() != theSecret {
		t.Errorf("resolved the wrong material from the environment")
	}
}

// TestResolveRefusesAnEmptyAnswer is the refusal that keeps a broken
// resolver from looking like a correctly-resolved empty passphrase, which
// downstream is an unopenable repository reported as a wrong password.
func TestResolveRefusesAnEmptyAnswer(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	for _, tc := range []struct {
		name string
		ref  secretref.Ref
	}{
		{"empty file", secretref.Ref{File: writeFile(t, "empty", "")}},
		{"whitespace-only file", secretref.Ref{File: writeFile(t, "blank", "  \n\t\n")}},
		{"command that prints nothing", secretref.Ref{Command: []string{"/usr/bin/true"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if _, err := secretref.Resolve(ctx, tc.ref); !errors.Is(err, secretref.ErrEmpty) {
				t.Errorf("Resolve() = %v, want ErrEmpty", err)
			}
		})
	}
}

// TestResolveNeverEchoesTheMaterial is the leak assertion, run over every
// error path a resolver can take with real material in hand.
//
// The dangerous one is the file source: the shell-credentials text and the
// path are both in scope at the moment the refusal is built, and a %q of
// the wrong variable is a one-character review miss.
func TestResolveNeverEchoesTheMaterial(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	// A file whose contents are fine but which is far too large, so the
	// refusal happens with the material available.
	huge := writeFile(t, "huge", theSecret+strings.Repeat("x", 128<<10))

	// A command that prints the secret and then fails, so both the exit
	// status and the material are in scope.
	failing := secretref.Ref{Command: []string{"/bin/sh", "-c", "echo " + theSecret + "; exit 3"}}

	for _, tc := range []struct {
		name string
		ref  secretref.Ref
	}{
		{"oversized file", secretref.Ref{File: huge}},
		{"failing command", failing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			secret, err := secretref.Resolve(ctx, tc.ref)
			if err == nil {
				t.Fatalf("Resolve did not refuse")
			}

			assertNoSecret(t, "error", err.Error())
			assertNoSecret(t, "%v of the secret returned beside the error", fmt.Sprintf("%v", secret))
		})
	}
}

// TestRefStringNamesTheSourceNeverTheMaterial matters because a Ref is
// exactly the thing that IS safe to log, and the whole custody story
// depends on it staying that way: a Ref carrying a resolved value would
// have every location that logs a repository location logging a passphrase.
func TestRefStringNamesTheSourceNeverTheMaterial(t *testing.T) {
	t.Parallel()

	path := writeFile(t, "passphrase", theSecret)

	for _, tc := range []struct {
		name string
		ref  secretref.Ref
		want string
	}{
		{"file", secretref.Ref{File: path}, "file " + path},
		{"env", secretref.Ref{Env: "SOME_VAR"}, "env SOME_VAR"},
		{"command", secretref.Ref{Command: []string{"/bin/vault", "read", "secret"}}, "command /bin/vault"},
		{"unset", secretref.Ref{}, "no source"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := tc.ref.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

// assertNoSecret fails when text contains the material this package is
// entrusted with, or any prefix of it long enough to be useful.
func assertNoSecret(t *testing.T, label, text string) {
	t.Helper()

	if strings.Contains(text, theSecret) {
		t.Errorf("%s contains the resolved secret: %s", label, text)
	}

	// "in whole or in part": a prefix long enough to narrow a brute force
	// is a leak too, and truncation is exactly how one gets there.
	if prefix := theSecret[:12]; strings.Contains(text, prefix) {
		t.Errorf("%s contains a %d-character prefix of the resolved secret: %s", label, len(prefix), text)
	}
}
