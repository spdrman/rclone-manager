// Verification that proves the path rather than the port (issue #592).
//
// # What one boolean cannot say
//
// TestConnection has answered with OK and a sentence since #146. The
// sentence is "could not connect and list the remote path", and it covers
// four completely different problems:
//
//  1. the host or port is wrong, or nothing is listening
//  2. the server offered a host key this backup set does not trust
//  3. this key's public half is not in the remote authorized_keys
//  4. the account authenticates and cannot read the folder
//
// Each has a different fix and two of them are not even on the same
// machine. Collapsing them is how an operator ends up regenerating a
// keypair to fix a chmod: 3 and 4 look identical from the outside, and 4
// is the only one that has nothing to do with keys at all.
//
// So a verification reports four results with four timings, and stops at
// the first failure. Stopping is not an optimisation. Reporting an
// authentication result past a host key that did not match would mean
// this process had authenticated to something it could not identify,
// which is the exact act the trust model exists to prevent.
//
// # Why the first two stages are measured here and the last two are not
//
// Stages 1 and 2 happen before the first authentication packet, so this
// package can perform them itself with no key material anywhere near this
// process: a dial, then a key exchange with NO auth methods offered. The
// host key callback is knownhosts' own, over the same candidate line the
// transport is about to be handed, so this is not a second opinion about
// a host key. It is the same comparison backupsethostkey.go's
// pinnedHostKeys makes, run against the file the transport will verify
// against, and a second opinion about host keys is one that can be wrong
// in the permissive direction.
//
// Stages 3 and 4 ride the real transport, deliberately: internal/
// transport.Transport.List is the same call a real cycle's discovery step
// makes, so a green verification is evidence about the thing that will
// actually run, and rclone opens the key file itself so key material
// still never enters this process. Which of the two failed is read off
// the adapter's own FR-22 category, never off the error's text, which is
// failure-safety invariant 12.
//
// # Nothing here is persisted
//
// The candidate known_hosts line goes to a temporary file for the
// duration of one check and is removed before returning, and no stage
// writes to the remote. Verifying settles nothing: only the acknowledged
// patch at the end does, and backupsethostkey.go's refusal is what stands
// between a changed host key and being trusted. A green verification is
// not consent and does not become one.
package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// The four claims a verification makes, in the order they are proven.
// They are machine-readable values a client renders, not sentences: what
// a moment is worth calling belongs to whichever surface is presenting
// it, the same argument liveactivity.go already makes.
const (
	ConnectionStageConnect      = "connect"
	ConnectionStageHostKey      = "host_key"
	ConnectionStageAuthenticate = "authenticate"
	ConnectionStageList         = "list"
)

// The three states a stage can be in. "skipped" is a real result and not
// an absence: a stage that was never attempted because an earlier one
// failed is a very different thing from a stage that failed, and a client
// that could not tell them apart would render three reds for one problem.
const (
	ConnectionStagePassed  = "passed"
	ConnectionStageFailed  = "failed"
	ConnectionStageSkipped = "skipped"
)

// ConnectionTestStage is one of the four claims, with what it found and
// how long it took.
//
// Detail is always one of this package's own sanitized strings or a value
// the caller itself supplied, never a raw rclone or x/crypto error, for
// the reason ConnectionTestResult.Message's own doc gives: those can
// embed a path or a stack-shaped string, and this is rendered in a
// browser and copied into support conversations.
type ConnectionTestStage struct {
	Name     string
	Status   string
	Detail   string
	Duration time.Duration
}

// verificationTimeouts are per stage rather than one budget over all
// four, so a slow dial cannot eat the time the listing needs and be
// reported as a listing failure.
const (
	connectDialTimeout = 10 * time.Second
	handshakeTimeout   = 10 * time.Second
	listTimeout        = 10 * time.Second
)

// runVerification performs the four stages against a candidate and
// reports every one of them.
//
// knownHostsPath is the temporary file holding the candidate line, and
// list is the last stage, handed in as a closure so this function has no
// opinion about the transport or the source it is built from.
func runVerification(ctx context.Context, host string, port int, user, knownHostsPath string, list func(context.Context) error) []ConnectionTestStage {
	if port <= 0 {
		port = defaultSSHPort
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))

	stages := []ConnectionTestStage{
		{Name: ConnectionStageConnect, Status: ConnectionStageSkipped},
		{Name: ConnectionStageHostKey, Status: ConnectionStageSkipped},
		{Name: ConnectionStageAuthenticate, Status: ConnectionStageSkipped},
		{Name: ConnectionStageList, Status: ConnectionStageSkipped},
	}

	// ---------------------------------------------------------- 1. dial ---
	started := time.Now()
	dialer := net.Dialer{Timeout: connectDialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	stages[0].Duration = time.Since(started)
	if err != nil {
		stages[0].Status = ConnectionStageFailed
		stages[0].Detail = "nothing answered on " + addr + ": " + dialProblem(err)
		return stages
	}
	defer func() { _ = conn.Close() }()
	stages[0].Status = ConnectionStagePassed
	stages[0].Detail = "tcp " + addr

	// ------------------------------------------------------ 2. host key ---
	//
	// The identification string is read off the wire first and replayed
	// into the handshake, because it is the one piece of evidence that
	// says something answered SSH rather than merely accepted a socket,
	// and the handshake below will not report it: this client offers no
	// auth methods, so it never completes.
	banner, replay := readIdentification(conn)
	if banner != "" {
		stages[0].Detail += " · " + banner
	}

	check, err := knownhosts.New(knownHostsPath)
	if err != nil {
		stages[1].Status = ConnectionStageFailed
		stages[1].Detail = "the host key line offered is not a usable known_hosts entry"
		return stages
	}

	var (
		offered    ssh.PublicKey
		hostKeyErr error
	)
	started = time.Now()
	_, _, _, handshakeErr := ssh.NewClientConn(replay, addr, &ssh.ClientConfig{
		User: user,
		// No auth methods, on purpose. The host key is presented during
		// the key exchange, which completes before authentication is ever
		// attempted, so this stage is measured without this process
		// holding a private key at all. The handshake is EXPECTED to fail
		// immediately afterwards, and that failure is not a result: it is
		// the shape of a probe that deliberately cannot log in.
		Auth: nil,
		HostKeyCallback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			offered = key
			hostKeyErr = check(hostname, remote, key)
			return hostKeyErr
		},
		Timeout: handshakeTimeout,
	})
	stages[1].Duration = time.Since(started)

	switch {
	case hostKeyErr != nil:
		stages[1].Status = ConnectionStageFailed
		stages[1].Detail = hostKeyMismatchDetail(addr, offered, hostKeyErr)
		return stages
	case offered == nil:
		// The key exchange never got as far as presenting a host key, so
		// nothing was compared. Refused rather than waved through, for
		// the reason unreadableTrustRefusal gives next door: proceeding
		// on "I could not check" is the one outcome an attacker would
		// choose.
		stages[1].Status = ConnectionStageFailed
		stages[1].Detail = "the server did not complete an SSH key exchange, so its identity was never offered: " + dialProblem(handshakeErr)
		return stages
	}
	stages[1].Status = ConnectionStagePassed
	stages[1].Detail = describeHostKey(offered)

	// -------------------------------------- 3. authenticate, 4. listing ---
	//
	// One call for two stages, because the sftp session that lists a
	// folder is the same session that authenticated to open it, and
	// splitting it would mean authenticating twice and reporting a
	// verification of something other than what runs. Which stage failed
	// is read off the adapter's own category rather than off the error's
	// text.
	listCtx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	started = time.Now()
	listErr := list(listCtx)
	elapsed := time.Since(started)

	if listErr == nil {
		stages[2].Status = ConnectionStagePassed
		stages[2].Detail = "publickey as " + user
		stages[3].Status = ConnectionStagePassed
		stages[3].Duration = elapsed
		return stages
	}

	category, _ := transport.CategoryOf(listErr)
	switch category {
	case transport.Authentication:
		stages[2].Status = ConnectionStageFailed
		stages[2].Duration = elapsed
		stages[2].Detail = "the server refused this key for " + user + ". Its public half is not in that account's authorized_keys on " + host
	case transport.KeyPermissions:
		stages[2].Status = ConnectionStageFailed
		stages[2].Duration = elapsed
		stages[2].Detail = "this deployment refused to use the selected key: its file permissions are no longer the 0600 it was stored with"
	case transport.HostVerification:
		// The stage above already passed against the same file, so this
		// is the transport disagreeing with the comparison this function
		// just made. Reported where it happened rather than silently
		// re-scored, because a disagreement about a host key is the one
		// thing here nobody should have to reconstruct from two greens.
		stages[1].Status = ConnectionStageFailed
		stages[1].Detail = "the sftp session was refused on the host key even though the key exchange above matched; the trusted line may have changed underneath this check"
	default:
		stages[2].Status = ConnectionStagePassed
		stages[2].Detail = "publickey as " + user
		stages[3].Status = ConnectionStageFailed
		stages[3].Duration = elapsed
		stages[3].Detail = listProblem(category, user)
	}
	return stages
}

// hostKeyMismatchDetail names both fingerprints, because those two
// strings are the entire content of the decision. It is the same sentence
// backupsethostkey.go's hostKeyChangeRefusal is built around, and for the
// same reason: an operator deciding whether this is their rebuilt server
// or somebody else's has nothing else to compare, and a message naming
// only the new one would be asking them to confirm something they cannot
// check.
func hostKeyMismatchDetail(addr string, offered ssh.PublicKey, err error) string {
	var keyErr *knownhosts.KeyError
	if errors.As(err, &keyErr) && len(keyErr.Want) > 0 {
		var want []string
		for _, k := range keyErr.Want {
			want = append(want, describeHostKey(k.Key))
		}
		return fmt.Sprintf(
			"%s offered %s, and the line being checked pins %s. A host key changes when a server is rebuilt and it changes in exactly the same way when something else is answering in its place. Compare the offered fingerprint against the host itself before you accept it",
			addr, describeHostKey(offered), strings.Join(want, ", "),
		)
	}
	if offered != nil {
		return addr + " offered " + describeHostKey(offered) + ", which the line being checked does not pin"
	}
	return "the host key offered by " + addr + " could not be checked against the line being verified"
}

// dialProblem says why a connection attempt failed in the shapes an
// operator can act on, and never by echoing an error whose text may carry
// a resolver's internals or a path.
func dialProblem(err error) string {
	switch {
	case err == nil:
		return "no reason was reported"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "it did not answer in time"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "it did not answer in time"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "that hostname could not be resolved"
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return "the connection was refused or unreachable"
	}
	return "the connection could not be established"
}

// listProblem turns the adapter's own category for a failed listing into
// a sentence about the source, which is where this failure actually is.
func listProblem(category transport.Category, user string) string {
	switch category {
	case transport.NotFound:
		return "the folder this set pulls from is not there on the source"
	case transport.PermissionDenied:
		return user + " authenticated and is not allowed to read the folder this set pulls from. That is a permissions problem on the source, and nothing to do with the key"
	case transport.Transient:
		return "the session dropped while listing the folder; the server is reachable and something interrupted the transfer"
	case transport.UnsupportedCapability:
		return "the account authenticated and the server would not serve sftp for it"
	default:
		return user + " authenticated and the folder this set pulls from could not be listed"
	}
}

// maxIdentificationBytes bounds what readIdentification will buffer. RFC
// 4253 caps an identification string at 255 bytes; this is generous over
// that and, more importantly, bounded at all, so a socket that answers
// with an endless stream of bytes and no newline cannot make this hold it.
const maxIdentificationBytes = 512

// readIdentification reads the server's SSH identification string and
// returns a net.Conn that replays it, so the handshake sees the stream
// exactly as it was sent.
//
// It reads at most one line, under a short deadline, and gives up
// silently: this is a nicety for the operator ("SSH-2.0-OpenSSH_9.6p1" is
// how you know you reached sshd and not a load balancer), never a
// verdict. Anything that goes wrong here leaves the banner empty and the
// stream untouched.
func readIdentification(conn net.Conn) (string, net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()

	buf := make([]byte, 0, 64)
	one := make([]byte, 1)
	for len(buf) < maxIdentificationBytes {
		n, err := conn.Read(one)
		if n > 0 {
			buf = append(buf, one[0])
			if one[0] == '\n' {
				break
			}
		}
		if err != nil {
			break
		}
	}
	replay := &replayConn{Conn: conn, pending: io.MultiReader(bytes.NewReader(buf), conn)}
	line := strings.TrimRight(string(buf), "\r\n")
	if !strings.HasPrefix(line, "SSH-") {
		// Not an SSH identification string. Not reported as a failure
		// here: the handshake below is what decides that, and it says so
		// with far better words than a guess made from a prefix.
		return "", replay
	}
	return line, replay
}

// replayConn is a net.Conn whose reads start with bytes already taken off
// the wire. Everything else is the underlying connection's, including
// Close, so the deferred close above still closes the socket.
type replayConn struct {
	net.Conn
	pending io.Reader
}

func (c *replayConn) Read(p []byte) (int, error) { return c.pending.Read(p) }
