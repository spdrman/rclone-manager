// What a server on the other end of a connection test can put on an
// operator's screen (EPIC G review).
//
// The connect step reports the SSH identification string the far end
// sent, and that is the right thing to report: it is the one piece of
// evidence that says something answered SSH rather than merely accepted
// a socket. It was reported verbatim. readIdentification took anything
// starting with "SSH-", trimmed the trailing CR/LF and appended up to
// 512 bytes of it to the check's Detail, which reaches the API response,
// the obs event and `activity --follow`'s stdout, which is usually a
// real TTY.
//
// RFC 4253 restricts that string to printable US-ASCII and nothing was
// enforcing it, so a server could send escape sequences and clear the
// scrollback an operator was reading as evidence, or scroll their own
// lines out of sight above it. The browser panel is not the exposure
// (React escapes it); the terminal is. And reaching it needs an operator
// to point a test connection at a host somebody else controls, which is
// exactly what the candidate-mode wizard invites during setup.
package sourcecheck

import (
	"context"
	"net"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// bannerServer is a plain TCP listener that answers with one line and
// then hangs up.
//
// Not an SSH server: what happens after the identification string is
// this test's business only insofar as it must not hang, and the
// handshake failing is the honest outcome for a socket that is not an
// SSH server. It closes rather than going quiet because
// ssh.NewClientConn puts no deadline on a handshake it did not dial, so
// a server that simply says nothing more would leave every case here
// waiting forever. The connect step has already recorded its detail by
// then, which is the whole of what this file is about.
func bannerServer(t *testing.T, banner string) (host string, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			go func(c net.Conn) {
				// No t.* calls: this goroutine outlives the test body.
				_, _ = c.Write([]byte(banner + "\r\n"))
				_ = c.Close()
			}(conn)
		}
	}()

	addr := ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", addr.Port
}

func bannerReport(t *testing.T, banner string) Report {
	t.Helper()
	host, port := bannerServer(t, banner)
	return Run(context.Background(), Target{
		BackupSetID: "api-server/var-backups", Host: host, Port: port,
		User: "backups", RemotePath: "/var/backups", KeyReference: "ssh_key_2",
	}, Deps{
		Credentials: func(context.Context) (string, error) { return "SHA256:aClientKeyFingerprint", nil },
		Trusted:     func(context.Context) ([]TrustedKey, error) { return nil, nil },
		Verify:      func(string, net.Addr, ssh.PublicKey) error { return nil },
		List:        func(context.Context) (int, error) { return 0, nil },
	})
}

// bannerIn is the part of a connect detail that came off the wire.
//
// The detail is this package's own sentence with the identification
// string appended after a separator, and the separator is a middle dot
// this package chose to write, so a claim about "printable US-ASCII"
// belongs to the half a remote server controls rather than to the whole
// line.
func bannerIn(t *testing.T, detail string) string {
	t.Helper()
	_, banner, found := strings.Cut(detail, " \u00b7 ")
	if !found {
		t.Fatalf("the connect detail is %q and carries no identification string at all", detail)
	}
	return banner
}

// assertPrintableASCII is RFC 4253's own restriction on the
// identification string, held mechanically. Everything an escape
// sequence needs is below 0x20.
func assertPrintableASCII(t *testing.T, banner string) {
	t.Helper()
	for i := 0; i < len(banner); i++ {
		if c := banner[i]; c < 0x20 || c > 0x7e {
			t.Fatalf("the reported banner carries byte %#02x at offset %d. It is printed straight into `activity --follow`'s stdout, which is usually a real TTY, so anything the far end sends outside printable US-ASCII is a remote server deciding what an operator's screen does: %q",
				c, i, banner)
		}
	}
}

// TestRun_AHostileBannerCannotDriveATerminal is the claim, driven by a
// server sending exactly what the review demonstrated: an erase-display,
// a cursor-home and a bell, wrapped in a banner that otherwise looks
// like a real OpenSSH one.
func TestRun_AHostileBannerCannotDriveATerminal(t *testing.T) {
	const hostile = "SSH-2.0-OpenSSH_9.6p1\x1b[2J\x1b[H\a nothing to see here"

	banner := bannerIn(t, checkFor(t, bannerReport(t, hostile), StepConnect).Detail)

	assertPrintableASCII(t, banner)
	// Escaped, not dropped. The banner is evidence, and an operator
	// investigating a host that answered like this needs to see that it
	// tried to clear their screen rather than a line with a hole in it.
	if !strings.Contains(banner, `\x1b`) || !strings.Contains(banner, `\a`) {
		t.Errorf("the reported banner is %q; the escapes the server sent should be visible as text rather than silently removed", banner)
	}
	// And the readable part still reads, because that is what the
	// identification string is reported for at all.
	if !strings.Contains(banner, "SSH-2.0-OpenSSH_9.6p1") {
		t.Errorf("the reported banner is %q and no longer names the server that answered", banner)
	}
	if !strings.Contains(banner, "nothing to see here") {
		t.Errorf("the reported banner is %q; the printable text either side of an escape is still what the far end said", banner)
	}
}

// A banner that is already what RFC 4253 says it should be comes through
// untouched, so this guard costs an honest server nothing.
func TestRun_AWellBehavedBannerIsUnchanged(t *testing.T) {
	const honest = "SSH-2.0-OpenSSH_9.6p1 Ubuntu-3ubuntu13.5"

	banner := bannerIn(t, checkFor(t, bannerReport(t, honest), StepConnect).Detail)

	if banner != honest {
		t.Errorf("the reported banner is %q, want %q verbatim", banner, honest)
	}
}

// A banner carrying bytes outside ASCII is neither thrown away nor
// passed through: it is shown, and every byte of it is printable.
func TestRun_ANonASCIIBannerIsShownRatherThanDropped(t *testing.T) {
	const accented = "SSH-2.0-OpenSSH_9.6p1 Gr\u00fc\u00dfe"

	banner := bannerIn(t, checkFor(t, bannerReport(t, accented), StepConnect).Detail)

	assertPrintableASCII(t, banner)
	if !strings.Contains(banner, "SSH-2.0-OpenSSH_9.6p1 Gr") {
		t.Errorf("the reported banner is %q and lost the part of the banner that was always printable", banner)
	}
	if !strings.Contains(banner, `\u00fc`) {
		t.Errorf("the reported banner is %q; a byte the far end sent that cannot be printed as itself is shown rather than dropped, so an operator can see it was there", banner)
	}
}
