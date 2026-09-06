// This file is one test, and it exists as its own file because of where it
// came from: it is the destructive-testing sweep of issue A213, which went
// looking for inputs the rest of the suite had not thought to stage.
//
// Keeping that provenance visible is the point. The two shapes here, an
// absolute path and an embedded control character, are not things a POSIX
// filesystem produces on its own, so they cannot arrive through the
// real-adapter tests in discovery_test.go and would look like paranoia
// sitting next to them. They belong to the contract instead: any future
// transport.Transport is forbidden from smuggling either past discovery,
// and this is where that obligation is written down as a test.
package discovery

import (
	"context"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// TestDiscover_AbsoluteAndControlCharacterPathsAreRejectedNotIngested rounds
// out this package's existing hostile-basename and traversal-shaped-path
// coverage (TestDiscover_HostileBasenameIsRejectedNotIngested,
// TestDiscover_TraversalShapedPathIsRejectedNotIngested) with the two
// remaining shapes isCleanRelativePath explicitly rejects: a path a
// malicious or buggy backend hands back as absolute (walking straight past
// whatever root a caller configured), and a path carrying an embedded
// control character (NUL, CR, LF), which no real POSIX filesystem could
// produce on its own but which any future transport.Transport
// implementation is still contractually forbidden from smuggling through
// discovery uningested. A legitimate, ordinary artifact is discovered
// alongside all of them, as a positive control that these are rejections,
// not a batch failure.
func TestDiscover_AbsoluteAndControlCharacterPathsAreRejectedNotIngested(t *testing.T) {
	fake := &fakeTransport{
		artifacts: []transport.RemoteArtifact{
			{Path: "/etc/passwd", Size: 1, ModTime: epoch.Unix()},
			{Path: "backup.dump\x00.txt", Size: 1, ModTime: epoch.Unix()},
			{Path: "backup\ndump.txt", Size: 1, ModTime: epoch.Unix()},
			{Path: "legitimate.dump", Size: 1, ModTime: epoch.Unix()},
		},
	}
	source := transport.Source{ID: "hostile-absolute", Type: "local", Root: "/unused"}
	set := backupSet(t, config.Completion{Strategy: "rename"}, nil)
	deps := Deps{Transport: fake, Journal: openJournal(t), Now: fixedNow(epoch)}

	res, err := Discover(context.Background(), deps, source, set)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	if len(res.Discovered) != 1 || res.Discovered[0].RemotePath != "legitimate.dump" {
		t.Fatalf("Discovered = %+v, want exactly legitimate.dump", res.Discovered)
	}
	if len(res.Rejected) != 3 {
		t.Fatalf("Rejected = %+v, want exactly 3 entries (absolute, NUL, newline)", res.Rejected)
	}
	for _, r := range res.Rejected {
		if r.RemotePath == "legitimate.dump" {
			t.Fatalf("the legitimate artifact was rejected: %+v", r)
		}
	}
}
