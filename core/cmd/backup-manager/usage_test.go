// Whether the usage block still names every verb the binary can run.
//
// It is a small file guarding a failure that has already happened: a verb
// that is dispatchable but unlisted is undiscoverable to an operator and
// invisible to the black-box guard in the tests repository that reads verbs
// out of this text, so nothing anywhere notices it shipped. `backup-set
// remove` went out exactly that way.
//
// The verbs come from the command's own dispatch rather than from a list
// typed here, which is what makes this a check instead of a second list to
// keep in step. Adding a verb over there is checked here without anybody
// remembering this file exists.
package main

import (
	"strings"
	"testing"
)

// TestUsage_NamesEveryBackupSetVerb. The usage block is what `--help`,
// `help` and an unknown command print, and it is also what a black-box
// guard in the tests repo reads verbs out of. A verb absent from it is
// undiscoverable to an operator and invisible to that guard, so the
// guard can never notice the verb landing. `backup-set remove` shipped
// exactly that way (issue #391).
//
// The verbs are read off cmdBackupSet's own dispatch (the switch and
// backupSetVerbs) rather than typed here again, so the next verb added
// there is checked without anyone remembering this test exists.
func TestUsage_NamesEveryBackupSetVerb(t *testing.T) {
	verbs := backupSetVerbNames()
	if len(verbs) < 4 {
		t.Fatalf("backupSetVerbNames() = %v, want at least create, patch, remove and retention", verbs)
	}

	out := captureStderr(t, usage)
	for _, verb := range verbs {
		if !strings.Contains(out, "backup-set "+verb+" ") {
			t.Errorf("usage() does not list \"backup-set %s\"; an operator cannot discover it and the black-box verb guard cannot see it", verb)
		}
	}
}
