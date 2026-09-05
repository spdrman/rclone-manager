package machines

import (
	"os"
	"testing"
)

// Whether two machines started by the same test process are genuinely two
// identities.
//
// The bug it holds the line on (#250) is gone by construction rather than by
// repair: no key is baked into an image any more, so two machines cannot
// inherit one another's authorized_keys through a shared tag. That is
// exactly the kind of claim that stops being true quietly, which is why it
// is a test and not a note in the harness.
//
// It asserts at the protocol level, and it puts its positive control first,
// because both of the cheap ways to write this pass for the wrong reason: two
// distinct key files can still both be authorized on both servers, and a
// machine that refuses everybody produces the same refusal as a machine that
// is properly isolated.

// TestTwoSourceMachinesDoNotShareAClientKey is #250, held under the
// mechanism that replaced the one it was filed against.
//
// The version of this harness that lived in ssh_test.go baked its freshly
// generated client key into an image and tagged it. A fixed tag is shared
// mutable state on the docker daemon, and this machine runs several test
// processes against one daemon as a matter of course, so a container one
// process started could be running another process's authorized_keys and
// would genuinely, permanently refuse the first process's key. That is not
// a startup race and no amount of waiting fixes it: it presented as "ssh:
// unable to authenticate, attempted methods [none publickey]" against a
// server whose own log said it was listening, and it cleared on an isolated
// re-run because nothing was then competing for the tag.
//
// The class of bug is structurally gone, because no key is baked into any
// image any more: the image is the same for every machine and every key is
// bind-mounted per container. This test is what holds that structural
// claim, because "structurally impossible" is exactly the kind of statement
// that quietly stops being true.
//
// It asserts at the protocol level rather than by comparing files. Two
// different files would satisfy a byte comparison while both being
// authorized on both servers, which is the failure being ruled out.
func TestTwoSourceMachinesDoNotShareAClientKey(t *testing.T) {
	m := Start(t)
	a := m.Source(t)
	b := m.AnotherSource(t)

	if a.KeyFile == b.KeyFile {
		t.Fatalf("both machines were handed the same client key file (%s), so they are not independent identities at all", a.KeyFile)
	}
	keyA, err := os.ReadFile(a.KeyFile)
	if err != nil {
		t.Fatalf("reading the first machine's key: %v", err)
	}
	keyB, err := os.ReadFile(b.KeyFile)
	if err != nil {
		t.Fatalf("reading the second machine's key: %v", err)
	}
	if string(keyA) == string(keyB) {
		t.Fatal("the two machines were given byte-identical client keys, which is the shared-image bug wearing different filenames")
	}

	// The positive control, and it comes first: each key authenticates
	// against its own machine. Without it, the refusal below is also what a
	// machine that refuses everybody would say.
	if err := waitForSSHAuth(a.Addr(), clientConfigFor(t, a.KeyFile, a.User), sshReadyWindow); err != nil {
		t.Fatalf("the first machine refused its OWN key, so nothing about key isolation can be read off it: %v", err)
	}
	if err := waitForSSHAuth(b.Addr(), clientConfigFor(t, b.KeyFile, b.User), sshReadyWindow); err != nil {
		t.Fatalf("the second machine refused its OWN key: %v", err)
	}

	// And the assertion. One attempt, not a retry loop: the question is
	// whether this server will authenticate a key it was never given, and
	// the answer does not improve with waiting.
	if err := trySSHHandshake(b.Addr(), clientConfigFor(t, a.KeyFile, b.User)); err == nil {
		t.Fatal("the second machine authenticated the FIRST machine's client key. The two machines share an identity, which is #250: a test that thought it was proving something about its own server would be talking to a server that trusts somebody else's key.")
	}
}
