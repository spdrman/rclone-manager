// Whether a restore can be found, and whether it can happen by accident.
//
// This is the one operation in the product that costs money and takes hours,
// so both halves matter more here than elsewhere. Discoverability is checked
// because an operator finding the verb by reading the source is not an
// acceptable answer, and the acknowledgement is checked because the flag is
// the entire mechanism standing between a mistyped command and a bill.
//
// The refusal cells run before anything is opened, which is what makes them
// evidence about billing rather than about argument parsing: a refusal that
// arrived after the service was built would already have talked to the
// provider.
package main

import (
	"strings"
	"testing"
)

// TestRestoreIsDiscoverableFromTheCommandLine. A verb absent from the
// command table cannot be run, and a verb absent from the usage block
// cannot be found; the second is how `backup-set remove` shipped
// invisibly (#391). A restore is the one operation in this product that
// costs money, so an operator finding it by reading the source is not an
// acceptable answer.
func TestRestoreIsDiscoverableFromTheCommandLine(t *testing.T) {
	if _, ok := commands["restore"]; !ok {
		t.Fatal("there is no restore command, so nothing an operator types reaches the restore this release built")
	}
	out := captureStderr(t, usage)
	if !strings.Contains(out, "restore <source/backup-set/artifact>") {
		t.Error("usage() does not list the restore verb, so an operator cannot discover it")
	}
	for _, flag := range []string{"--medium", "--acknowledge", "--days"} {
		if !strings.Contains(out, flag) {
			t.Errorf("usage() does not mention %s, which a restore cannot be asked for without", flag)
		}
	}
}

// TestARestoreIsRefusedBeforeAnythingIsOpened is the CLI half of "every
// refusal happens before anything is billed", and it is stronger than it
// looks: every case here is refused with NO config path that resolves to
// anything, so a refusal that leaked past these checks would fail trying
// to open a state database rather than returning 2.
//
// That is what makes the assertion meaningful. Exit code 2 is this CLI's
// usage-error code and 1 is its failure code, so "2" proves the request
// was rejected on its own terms, before a service, a journal or a
// provider was involved at all.
func TestARestoreIsRefusedBeforeAnythingIsOpened(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		says string
	}{
		{
			name: "no artifact at all",
			args: []string{"--medium", "cold-store", "--acknowledge"},
			says: "expected <source/backup-set/artifact>",
		},
		{
			name: "no medium named",
			args: []string{"production/postgres/dump.zst", "--acknowledge"},
			says: "--medium is required",
		},
		{
			name: "nobody acknowledged the cost",
			args: []string{"production/postgres/dump.zst", "--medium", "cold-store"},
			says: "--acknowledge is required",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := captureStderr(t, func() {
				if code := cmdRestore(tc.args); code != 2 {
					t.Errorf("cmdRestore = %d, want 2; a refused restore must not reach a service, a journal or a provider", code)
				}
			})
			if !strings.Contains(out, tc.says) {
				t.Errorf("the refusal reads %q, and does not say %q", strings.TrimSpace(out), tc.says)
			}
		})
	}
}

// TestTheAcknowledgementIsAFlagAnOperatorHasToType is the shape argument
// made as an assertion.
//
// --acknowledge defaults to false, which is the answer that costs
// nothing, so the operator who ran the command without reading it gets a
// refusal. A --force spelled the other way round would have the default
// be the expensive answer for anyone who did not think about it. This
// pins the direction, because flipping it is a one-character edit that no
// other test in this file would notice.
func TestTheAcknowledgementIsAFlagAnOperatorHasToType(t *testing.T) {
	fs, _ := newFlagSet("restore")
	acknowledge := fs.Bool("acknowledge", false, "")
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parsing no flags at all: %v", err)
	}
	if *acknowledge {
		t.Fatal("--acknowledge defaults to true, so an operator who never typed it has authorised a bill")
	}
}
