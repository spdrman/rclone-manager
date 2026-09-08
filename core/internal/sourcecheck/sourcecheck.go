// Package sourcecheck is what `Test Connection` actually did (EPIC G,
// issue #596).
//
// # What was there before
//
// One boolean. BackupService.TestBackupSetConnection built a
// transport.Source out of the persisted set, made exactly one
// Transport.List call, threw the error away and answered
// {ok:false, message:"could not connect and list the remote path"}.
//
// Throwing the error away was right, and this package keeps doing it:
// rclone's and x/crypto/ssh's own text embeds transport internals a
// caller must not have to treat as safe to render. What was wrong is
// what was left. DNS, the TCP connect, the host key check, the key
// material, the authentication and the listing all collapsed into that
// one sentence, so a typo'd hostname, an unauthorised key, a rotated
// host key and a path that does not exist read identically. Those are
// four different afternoons.
//
// # It is mediumcheck's shape, deliberately
//
// core/internal/mediumcheck already had the contract this needed: a
// closed, ordered Steps list, an Outcome of passed/failed/skipped, and a
// Check{Step, Outcome, Category, Detail} where Category is the
// machine-readable half a surface branches on and Detail is one of the
// package's own sentences that never carries an underlying error's text.
// Copying that shape rather than inventing a second one is what lets
// `medium preflight`'s table and this one be two renderings of one idea.
//
// Skipped in particular had to survive the copy. mediumcheck's own
// argument transfers word for word with `authenticate` in it: a surface
// that renders a skipped authentication as anything but "this was never
// tried" has told an operator their credentials are fine on the strength
// of a step that never ran.
//
// # Why credentials comes first
//
// Same argument mediumcheck makes for putting StepCredentials ahead of
// StepReach: whether a credential can be obtained at all and whether the
// far side accepts it "are different jobs for a person, one is a file or
// a variable or a command on this host, the other is a policy at the
// provider". A key file this process cannot read is not a network
// problem and should never be reported as one.
//
// # One dial for the first four steps, and no dial at all for the last two
//
// StepConnect dials TCP once and KEEPS the socket. It reads the server's
// identification string off the wire ("SSH-2.0-OpenSSH_9.6p1") and
// replays it into the handshake StepHostKey then runs over that same
// socket, with NO auth methods offered: the host key is presented during
// key exchange, which completes before authentication is ever attempted,
// so the server's identity is established with no private key anywhere
// near this process. The banner is the one piece of evidence that says
// something answered SSH rather than merely accepted a socket, and the
// handshake cannot report it because it deliberately never completes.
//
// StepAuthenticate and StepList come out of ONE Deps.List call, split by
// the adapter's own transport.Category. This is not an optimisation, it
// is the only honest arrangement: the sftp session that lists a folder is
// the session that authenticated to open it, and a second dial holding
// this process's own copy of the private key would be reporting on a
// login no backup ever performs. rclone opens key_file itself, which is
// exactly why key_file exists, and this check never takes that away from
// it.
//
// # Trust is knownhosts', never this package's
//
// Deps.Verify is the host key decision and it is knownhosts' own
// callback over the same file the transport is about to be handed. This
// package never compares fingerprints to decide anything. It reads the
// trusted lines through Deps.Trusted only to SAY what was trusted when
// the decision has already gone against the server, because a mismatch is
// settled by an operator comparing fingerprints by eye and a callback
// answers yes or no without ever naming what it wanted.
//
// A comparison of its own would be a second opinion about a host key, and
// a second opinion can be wrong in the permissive direction: a marker
// (@revoked, @cert-authority) and a line's host patterns are both part of
// what a known_hosts entry MEANS, and a fingerprint comparison that reads
// only the key throws both away. That is how a revoked key reports as
// trusted and how a key pinned for a different host counts for this one.
//
// # It proves the same path a cycle takes
//
// StepList is not a hand-rolled SFTP walk. It is Deps.List, which the
// service wires to the same transport.Transport.List a real cycle's
// discovery step calls, against the same transport.Source. A check that
// reached the remote by some other route would be proving something
// about a code path no backup ever takes.
package sourcecheck

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// Step names one thing this check proves. The set is closed and ordered:
// Steps below is the order Run performs them in, and a Report always
// carries one Check per step so a surface renders a fixed list rather
// than discovering which ones happened to run.
type Step string

const (
	// StepCredentials is whether the key this backup set names can be
	// USED on this host at all: the reference resolves to something that
	// is there, and the file's own mode and its whole containing
	// directory chain are still what a real transfer demands before it
	// will touch the key (#293/#311). Nothing about the far side.
	//
	// It does not open the private half. Every check it makes is an
	// os.Stat, and the fingerprint in its detail is the PUBLIC half's,
	// named from this deployment's key store when the key is one this
	// deployment holds. A step that read the private key to name it would
	// be pulling key material into this process for a diagnostic, which
	// is the thing key_file exists to avoid.
	StepCredentials Step = "credentials"

	// StepResolve is whether the hostname resolves, and to what. Its
	// detail names the addresses, because "it resolves" and "it resolves
	// to the machine you think" are different claims and only an
	// operator can settle the second.
	StepResolve Step = "resolve"

	// StepConnect is whether TCP to host:port answered, how fast, and
	// what identification string came back. The timing is in the detail
	// on purpose: a connect that succeeds in four seconds is a different
	// report from one that succeeds in twelve milliseconds, and neither
	// is a failure. The banner is there because "SSH-2.0-OpenSSH_9.6p1"
	// is how an operator knows they reached sshd and not a load balancer.
	StepConnect Step = "connect"

	// StepHostKey is which key the server offered, its SHA256
	// fingerprint, and whether the known_hosts file this backup set
	// verifies against actually trusts it FOR THIS HOST. The decision is
	// Deps.Verify, which is knownhosts' own callback over the same file
	// the transport will be handed; this package never makes a
	// fingerprint comparison of its own to decide it.
	//
	// Both fingerprints go in the detail in `ssh-keygen -lf` form when
	// the answer is no, so an operator can compare them by eye against an
	// out-of-band source, which is the only thing that actually settles a
	// mismatch.
	StepHostKey Step = "host_key"

	// StepAuthenticate is whether the server accepted that key for that
	// user. It is skipped when the host key did not match: offering a
	// credential to a server whose identity has not been established is
	// the thing FR-6 exists to prevent.
	//
	// It has no Duration of its own. It is decided from the same
	// Deps.List call StepList is, because the sftp session that lists a
	// folder is the session that authenticated to open it.
	StepAuthenticate Step = "authenticate"

	// StepList is the configured remote path listed over the real
	// transport, and how many entries came back. It is the only step
	// that proves the path in the configuration is a path on that
	// machine, and, like StepAuthenticate, it carries no Duration of its
	// own: the two share one call and neither has a timing that is only
	// about itself.
	StepList Step = "list"
)

// Steps is every step, in the order Run performs them.
var Steps = []Step{
	StepCredentials, StepResolve, StepConnect,
	StepHostKey, StepAuthenticate, StepList,
}

// Outcome is what one step produced.
type Outcome string

const (
	// Passed: the step ran and proved what it is for.
	Passed Outcome = "passed"

	// Failed: the step ran and did not.
	Failed Outcome = "failed"

	// Skipped: the step never ran, because an earlier one failed in a way
	// that makes this one meaningless. It is a first-class outcome and
	// never a quiet pass; see the package doc.
	Skipped Outcome = "skipped"
)

// The categories a failure classifies as. They are the machine-readable
// half: a surface branches on one of these, never on a Detail sentence.
//
// They are this package's own small closed set rather than the transport
// package's, because the failures here happen at six different layers and
// only two of them ever reach a transport at all.
const (
	// CategoryCredentials: the key could not be obtained or opened on
	// this host. A local problem, whatever the far side thinks.
	CategoryCredentials = "credentials"

	// CategoryDNS: the hostname did not resolve.
	CategoryDNS = "dns"

	// CategoryNetwork: nothing answered on host:port, or the answer was
	// not an SSH server.
	CategoryNetwork = "network"

	// CategoryHostKey: the server offered a key this backup set does not
	// trust, or this set's known_hosts could not be read.
	CategoryHostKey = "host_key"

	// CategoryAuthentication: the server refused the credential.
	CategoryAuthentication = "authentication"

	// CategoryRemotePath: authentication succeeded and the configured
	// remote path could not be listed.
	CategoryRemotePath = "remote_path"
)

// Check is one step's result.
type Check struct {
	Step    Step
	Outcome Outcome

	// Category is one of the constants above, or empty when the step
	// passed or was skipped.
	Category string

	// Detail is one of this package's own sentences. It never carries an
	// underlying error's text: see the package doc.
	Detail string

	// Duration is how long this step took ON ITS OWN, and is ZERO when
	// there is no such number rather than when the step was instant.
	//
	// Only the steps that are measured separately carry one:
	// credentials, resolve, connect and host_key. StepAuthenticate and
	// StepList come out of a single Deps.List call and have no timing
	// that belongs to one of them, so they leave this at zero and every
	// surface downstream OMITS it rather than printing "0 ms" beside a
	// green row, which would tell an operator the server answered
	// instantly when what happened is that nobody measured.
	Duration time.Duration
}

// Report is one connection test.
type Report struct {
	// BackupSetID is the set this ran against.
	BackupSetID string

	// OK is true only when no check failed.
	OK bool

	// Checks is one entry per Step, in Steps order.
	Checks []Check
}

// StoppedAt returns the first failed check, and whether there was one. A
// surface that says "stopped at host_key" rather than "the test failed"
// reads this rather than filtering by hand.
func (r Report) StoppedAt() (Check, bool) {
	for _, c := range r.Checks {
		if c.Outcome == Failed {
			return c, true
		}
	}
	return Check{}, false
}

// Target is the connection this check is about, as the configuration
// already describes it. Nothing in it is a secret and nothing in it can
// become one: the key is reached through Deps.Credentials, which hands
// back a signer rather than key material.
type Target struct {
	BackupSetID string
	Host        string
	Port        int
	User        string
	RemotePath  string

	// KeyReference is how the configuration NAMES the key ("ssh_key_2",
	// "$BACKUP_KEY", "the configured command"), for the credentials
	// step's detail. Never the key, never a path this process resolved.
	KeyReference string

	// PassphraseConfigured says the key is passphrase-protected, so a
	// passing credentials step can say the passphrase opened it rather
	// than leaving an operator wondering whether it was even tried.
	PassphraseConfigured bool
}

// TrustedKey is one host key this backup set already trusts, read out of
// the known_hosts file its configuration points at.
type TrustedKey struct {
	Algorithm   string
	Fingerprint string
	// Line is the 1-based line number in that known_hosts file, so a
	// mismatch can name where the trusted answer came from.
	Line int
}

// Deps is what Run is handed. Every one of them is a real capability
// wired by the caller; there are no toggles here.
type Deps struct {
	// Credentials proves the key this backup set names can be used on
	// this host, WITHOUT opening its private half, and names that key's
	// public fingerprint when this deployment can.
	//
	// An empty fingerprint with a nil error is a real answer and not a
	// half-failure: it means the key is usable and this deployment does
	// not hold it in its own store, so there is nothing to read the
	// public half out of. Naming it anyway would mean opening the private
	// key to derive it, and the whole reason key_file is the documented
	// preference is that rclone opens the key and this process never
	// does.
	Credentials func(context.Context) (fingerprint string, err error)

	// Trusted reads the host keys this backup set's own known_hosts file
	// pins, for the mismatch DETAIL and for nothing else.
	//
	// It decides nothing. Verify below is the decision. This exists
	// because a callback answers yes or no without naming what it
	// wanted, and a mismatch is settled by an operator comparing the
	// offered fingerprint against the trusted one by eye.
	Trusted func(context.Context) ([]TrustedKey, error)

	// Verify is the host key decision: knownhosts.New(path) in
	// production, over the SAME known_hosts file the transport is about
	// to verify against.
	//
	// It is a dependency rather than a comparison this package makes
	// because knownhosts is the only reading that honours everything a
	// known_hosts line means: the marker (@revoked, @cert-authority), the
	// host patterns, negations, and hashed entries whose per-line salt
	// nothing else can reproduce. A fingerprint comparison throws all of
	// that away and it throws it away PERMISSIVELY, so the check would
	// pass on exactly the connections the transport then refuses.
	Verify func(hostname string, remote net.Addr, key ssh.PublicKey) error

	// List lists the configured remote path over the real transport and
	// reports how many entries came back. It is the one call that decides
	// both StepAuthenticate and StepList, split by transport.CategoryOf.
	List func(context.Context) (entries int, err error)

	// Observe, when set, is called once per failed step with the
	// underlying cause, so an operator's log keeps the diagnostic the
	// Report deliberately does not carry. Nil discards it. This mirrors
	// mediumcheck.Deps.Observe exactly.
	Observe func(step Step, err error)
}

func (d Deps) observe(step Step, err error) {
	if d.Observe != nil && err != nil {
		d.Observe(step, err)
	}
}

// connectTimeout bounds the TCP dial in StepConnect. A connection test is
// a question an operator is waiting on, so an unreachable host fails fast
// rather than holding a button down for a minute.
const connectTimeout = 5 * time.Second

// handshakeTimeout bounds the SSH key exchange in StepHostKey when the
// caller's context does not bound it sooner. Longer than the TCP dial
// because it covers a key exchange on both ends.
//
// It is applied as a deadline on the socket, and exchangeHostKey says why
// that is the only place it can go: ssh.NewClientConn reads no timeout and
// takes no context, so a deadline on the connection is the one thing that
// interrupts it.
const handshakeTimeout = 10 * time.Second

// maxIdentificationBytes bounds what readIdentification will buffer. RFC
// 4253 caps an identification string at 255 bytes; this is generous over
// that and, more importantly, bounded at all, so a socket that answers
// with an endless stream of bytes and no newline cannot make this hold it.
const maxIdentificationBytes = 512

// builder accumulates the Report as Run walks the steps, and is the one
// place the skip rule lives: once a step has failed, every later step
// that has not already run is skipped with a sentence saying WHY it was
// never tried rather than a bare "skipped".
type builder struct {
	target Target
	byStep map[Step]Check
}

func newBuilder(t Target) *builder {
	return &builder{target: t, byStep: map[Step]Check{}}
}

func (b *builder) pass(step Step, detail string) {
	b.byStep[step] = Check{Step: step, Outcome: Passed, Detail: detail}
}

func (b *builder) fail(step Step, category, detail string) {
	b.byStep[step] = Check{Step: step, Outcome: Failed, Category: category, Detail: detail}
}

// took records how long a step that is measured on its own took. It is
// called only for credentials, resolve, connect and host_key; the two
// steps that share one call never get one, which is what keeps "absent"
// and "zero" the same thing here and distinguishable downstream.
func (b *builder) took(step Step, d time.Duration) {
	c, ok := b.byStep[step]
	if !ok {
		return
	}
	c.Duration = d
	b.byStep[step] = c
}

func (b *builder) skip(step Step, detail string) {
	b.byStep[step] = Check{Step: step, Outcome: Skipped, Detail: detail}
}

// skipRest marks every step Run has not reached, with one shared reason.
// "never attempted" is the fallback for the steps further down the list
// than the one that gave the reason.
func (b *builder) skipRest(from Step, reason string) {
	seen := false
	for _, s := range Steps {
		if s == from {
			seen = true
		}
		if !seen {
			continue
		}
		if _, done := b.byStep[s]; done {
			continue
		}
		if reason != "" {
			b.skip(s, reason)
			reason = ""
			continue
		}
		b.skip(s, "never attempted")
	}
}

func (b *builder) report() Report {
	out := Report{BackupSetID: b.target.BackupSetID, OK: true}
	for _, s := range Steps {
		c, ok := b.byStep[s]
		if !ok {
			c = Check{Step: s, Outcome: Skipped, Detail: "never attempted"}
		}
		if c.Outcome == Failed {
			out.OK = false
		}
		out.Checks = append(out.Checks, c)
	}
	return out
}

// Run performs one connection test against target and reports what it
// found, one Check per Step, always in Steps order and always all six.
//
// It never returns an error. Every way this can go wrong is a fact about
// the operator's configuration or about the far side, which is an
// ORDINARY OUTCOME and belongs on a step, exactly as
// TestBackupSetConnection has always argued about its own boolean. A Go
// error here would be a claim that this manager broke, and none of these
// are that.
func Run(ctx context.Context, target Target, deps Deps) Report {
	b := newBuilder(target)

	started := time.Now()
	keyFingerprint, err := deps.Credentials(ctx)
	elapsed := time.Since(started)
	if err != nil {
		deps.observe(StepCredentials, err)
		b.fail(StepCredentials, CategoryCredentials, credentialsFailureDetail(target))
		b.took(StepCredentials, elapsed)
		b.skipRest(StepResolve, "the key this backup set names cannot be used on this host, so nothing was tried against "+target.Host)
		return b.report()
	}
	b.pass(StepCredentials, credentialsPassedDetail(target, keyFingerprint))
	b.took(StepCredentials, elapsed)

	started = time.Now()
	addresses, err := resolveHost(ctx, target.Host)
	elapsed = time.Since(started)
	if err != nil {
		deps.observe(StepResolve, err)
		b.fail(StepResolve, CategoryDNS, "this host does not resolve to an address from this machine: "+target.Host)
		b.took(StepResolve, elapsed)
		b.skipRest(StepConnect, "the hostname did not resolve, so there was no address to connect to")
		return b.report()
	}
	b.pass(StepResolve, target.Host+" is "+strings.Join(addresses, " and "))
	b.took(StepResolve, elapsed)

	addr := net.JoinHostPort(target.Host, strconv.Itoa(target.Port))
	started = time.Now()
	conn, err := dial(ctx, addr)
	elapsed = time.Since(started)
	if err != nil {
		deps.observe(StepConnect, err)
		b.fail(StepConnect, CategoryNetwork, "nothing answered TCP on "+addr+": "+dialProblem(err))
		b.took(StepConnect, elapsed)
		b.skipRest(StepHostKey, "nothing answered on "+addr+", so no host key was offered")
		return b.report()
	}
	// The socket stays open through the host key step below: the key
	// exchange that presents the host key runs over THIS connection, not
	// a second one.
	defer func() { _ = conn.Close() }()

	// Read off the wire before the handshake and replay it in, so the
	// handshake sees the stream exactly as it was sent. The banner is a
	// nicety and never a verdict: anything that goes wrong leaves it
	// empty and the stream untouched.
	banner, replay := readIdentification(conn)
	connectDetail := "TCP to " + addr + " in " + roundMillis(elapsed)
	if banner != "" {
		connectDetail += " · " + banner
	}
	b.pass(StepConnect, connectDetail)
	b.took(StepConnect, elapsed)

	offered, verifyErr, elapsed, handshakeErr := exchangeHostKey(ctx, replay, addr, target.User, deps.Verify)
	b.took(StepHostKey, elapsed)

	if offered == nil {
		// The key exchange never got as far as presenting a host key, so
		// nothing was compared. Refused rather than waved through:
		// proceeding on "I could not check" is the one outcome an
		// attacker would choose.
		deps.observe(StepHostKey, handshakeErr)
		b.fail(StepHostKey, CategoryNetwork, addr+" answered TCP but did not complete an SSH key exchange, so it is not offering a host key this manager can read")
		b.took(StepHostKey, elapsed)
		b.skipRest(StepAuthenticate, "no host key was read, so nothing was offered to this server")
		return b.report()
	}

	algorithm := offered.Type()
	fingerprint := ssh.FingerprintSHA256(offered)

	// Trusted is read for the SENTENCE, whichever way the decision went,
	// and its own failure never changes the decision. When it cannot be
	// read the wording says so instead of naming lines nobody has.
	trusted, trustedErr := deps.Trusted(ctx)
	if trustedErr != nil {
		deps.observe(StepHostKey, trustedErr)
		trusted = nil
	}

	if verifyErr != nil {
		deps.observe(StepHostKey, verifyErr)
		b.fail(StepHostKey, CategoryHostKey, hostKeyMismatchDetail(addr, algorithm, fingerprint, trusted, trustedErr, verifyErr))
		b.took(StepHostKey, elapsed)
		b.skipRest(StepAuthenticate, "the host key did not match, so nothing was offered to this server")
		return b.report()
	}
	b.pass(StepHostKey, hostKeyMatchDetail(algorithm, fingerprint, trusted))
	b.took(StepHostKey, elapsed)

	// ONE call for the last two steps. Which of them failed is read off
	// the adapter's own FR-22 category and never off the error's text.
	entries, listErr := deps.List(ctx)
	if listErr == nil {
		b.pass(StepAuthenticate, "the server accepted publickey for "+target.User)
		b.pass(StepList, remotePathOf(target)+" listed, "+plural(entries, "entry", "entries"))
		return b.report()
	}

	category, _ := transport.CategoryOf(listErr)
	switch category {
	case transport.Authentication:
		deps.observe(StepAuthenticate, listErr)
		b.fail(StepAuthenticate, CategoryAuthentication, "the server did not accept this key for "+target.User+"@"+target.Host+". On that machine, the public half has to be in "+target.User+"'s authorized_keys")
		b.skipRest(StepList, "authentication did not succeed, so the remote path was never listed")

	case transport.KeyPermissions:
		// The transport refused to USE the key, on the same mode and
		// directory-chain rule the credentials step checked a moment
		// ago. Reported where it happened rather than blamed on
		// authenticate: nothing was offered to the server at all, and
		// "the server refused your key" would send an operator to the
		// wrong machine. The credentials row is flipped rather than left
		// green, for the reason HostVerification below is: a
		// disagreement between two readings of the same fact is not
		// something anybody should have to reconstruct from a green.
		deps.observe(StepCredentials, listErr)
		b.fail(StepCredentials, CategoryCredentials, "this deployment refused to use this backup set's key when the transfer went to open it: its file permissions, or those of a directory containing it, are no longer what a key may be stored with")
		b.skipRest(StepAuthenticate, "the key was refused on this host before anything was offered to the server")

	case transport.HostVerification:
		// The step above already passed against the same file, so this
		// is the transport disagreeing with the comparison this check
		// just made. Reported where it happened rather than silently
		// re-scored: a disagreement about a host key is the one thing
		// here nobody should have to reconstruct from two greens.
		deps.observe(StepHostKey, listErr)
		b.fail(StepHostKey, CategoryHostKey, "the sftp session was refused on the host key even though the key exchange above matched; the trusted line may have changed underneath this check")
		b.took(StepHostKey, elapsed)
		b.skipRest(StepAuthenticate, "the transport refused the host key, so nothing was offered to this server")

	default:
		// Authentication is what a listing failure of any other shape
		// proves: the session got far enough to be refused on the PATH.
		deps.observe(StepList, listErr)
		b.pass(StepAuthenticate, "the server accepted publickey for "+target.User)
		b.fail(StepList, CategoryRemotePath, listProblem(category, target, remotePathOf(target)))
	}
	return b.report()
}

// exchangeHostKey runs the SSH key exchange over an already-open socket
// with NO auth methods offered, and reports the key the server presented,
// what verify made of it, and how long the exchange took.
//
// The handshake is EXPECTED to fail immediately after key exchange: this
// client cannot log in and is not trying to. That failure is not a
// result, it is the shape of a probe that deliberately holds no
// credential, which is why handshakeErr matters only when no key was
// presented at all.
//
// # Why the bound is a deadline on the socket
//
// ssh.NewClientConn takes an open net.Conn and no context, and it never
// reads ClientConfig.Timeout: in x/crypto only Dial reads that field, to
// bound the TCP connect Dial makes itself, and the field's own doc says
// so. An earlier version of this function set the field to
// handshakeTimeout and was bounded by nothing at all, so a peer that
// accepted the connection and then sent no identification string (a load
// balancer in front of a dead backend, an appliance answering on 22, an
// accept-and-drop firewall) left the exchange in a read that nothing
// would ever wake. The one caller that runs this check with a
// process-wide lock held, core/service.UpdateBackupSet, then held that
// lock until the process was restarted (PR #628 review).
//
// The only thing that interrupts NewClientConn is a deadline on the
// connection it is reading from, because that aborts the read itself. So
// the bound goes there: the caller's context deadline when it has one and
// it is sooner, handshakeTimeout otherwise, and a cancelled context
// becomes an immediate deadline the same way, because a handshake nobody
// is waiting on should stop rather than run out its clock holding
// whatever the caller holds. The deadline is cleared on the way out so
// nothing this function set outlives it on the socket.
func exchangeHostKey(ctx context.Context, conn net.Conn, addr, user string, verify func(string, net.Addr, ssh.PublicKey) error) (offered ssh.PublicKey, verifyErr error, elapsed time.Duration, handshakeErr error) {
	deadline := time.Now().Add(handshakeTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	defer func() { _ = conn.SetDeadline(time.Time{}) }()
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()

	started := time.Now()
	_, _, _, handshakeErr = ssh.NewClientConn(conn, addr, &ssh.ClientConfig{
		User: user,
		Auth: nil,
		HostKeyCallback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			offered = key
			verifyErr = verify(hostname, remote, key)
			return verifyErr
		},
		// No Timeout, deliberately: see above. Setting it would document
		// a bound this call does not have.
	})
	return offered, verifyErr, time.Since(started), handshakeErr
}

// remotePathOf is the path this set is configured to read, with the same
// default the service applies when a set names none.
func remotePathOf(t Target) string {
	if t.RemotePath == "" {
		return "/"
	}
	return t.RemotePath
}

// credentialsPassedDetail says what was actually proven, which is less
// than "the key works" and more than "a path exists": the reference
// resolves, and the permissions a real transfer insists on are still
// intact.
//
// An empty fingerprint is stated rather than left as a gap. "This
// deployment does not hold this key, so its public half cannot be named
// without opening the private one" is a FACT about how this set is
// configured, not a failure, and a detail that just went quiet there
// would read as though the naming had been attempted and lost.
func credentialsPassedDetail(t Target, fingerprint string) string {
	out := "the key " + keyReferenceOf(t) + " is where the configuration says, and its file and directory permissions are still what a transfer requires"
	if t.PassphraseConfigured {
		out += ". It is passphrase-protected, and the passphrase is proven by the authenticate step rather than here, because opening the key to try it is what this step exists not to do"
	}
	if fingerprint != "" {
		return out + ". Its public half is " + fingerprint + ", which is the string that has to appear in the far side's authorized_keys"
	}
	return out + ". Its public half cannot be named here: this deployment does not hold this key in its own store, and deriving the fingerprint would mean opening the private half"
}

func credentialsFailureDetail(t Target) string {
	return "the key " + keyReferenceOf(t) + " cannot be used on this host: it is not where the configuration says, or its permissions, or those of a directory containing it, are not what a key may be stored with"
}

func keyReferenceOf(t Target) string {
	if t.KeyReference == "" {
		return "this backup set is configured with"
	}
	return t.KeyReference
}

// hostKeyMatchDetail is the good news, and it still prints the
// fingerprint rather than the word "matched". An operator reading a log
// after a host was rebuilt wants to see WHICH key was accepted, and a
// panel that only ever says "matched" has nothing to show them.
//
// The line number is named when the trusted reading can find the offered
// key in the file and quietly left out when it cannot. It is decoration
// on a decision knownhosts already made: a hashed entry pins a key this
// reading cannot compare by fingerprint, and inventing "no line" there
// would contradict the pass immediately above it.
func hostKeyMatchDetail(algorithm, fingerprint string, trusted []TrustedKey) string {
	out := "the server offered " + algorithm + " " + fingerprint + ", and this backup set's known_hosts trusts it for this host"
	if match, ok := matchTrusted(trusted, fingerprint); ok {
		return out + " on line " + strconv.Itoa(match.Line)
	}
	return out
}

// hostKeyMismatchDetail names the offered key and every key the file
// pins, with line numbers, because those strings are the entire content
// of the decision an operator now has to make. A message naming only the
// new one would be asking them to confirm something they cannot check.
//
// The trusted lines are a REPORT of what is in the file, never the reason
// for the refusal. verifyErr already is that reason, and the wording
// distinguishes the two shapes knownhosts reports: a key pinned for this
// host that is a different key, and a key that is simply not pinned for
// this host at all, which is what a wrong-host line and a revoked line
// both come back as.
func hostKeyMismatchDetail(addr, algorithm, fingerprint string, trusted []TrustedKey, trustedErr, verifyErr error) string {
	var out strings.Builder
	var keyErr *knownhosts.KeyError
	var revoked *knownhosts.RevokedError
	switch {
	case errors.As(verifyErr, &revoked):
		// A line that exists to say NO. Named on its own because it is
		// the one refusal here that is not ambiguous: nobody has to go
		// and compare fingerprints out of band, the answer is already
		// written down, and a message that lumped it in with "this is
		// not the key we expected" would send an operator off to verify
		// a key somebody has already decided against.
		out.WriteString("this key is REVOKED in this backup set's known_hosts. A revoked line is not a stale one: it says this exact key must never be accepted for this host again, so re-pinning it is not the fix")
	case errors.As(verifyErr, &keyErr) && len(keyErr.Want) > 0:
		out.WriteString("the key " + addr + " offers is not the one this backup set trusts for it. A host key changes when a server is rebuilt and it changes in exactly the same way when something else is answering in its place, so compare the offered fingerprint against the host itself before you accept it")
	case errors.As(verifyErr, &keyErr):
		out.WriteString("this backup set's known_hosts does not trust this key for " + addr + ". Either nothing is pinned for this host, or the line that holds this key pins it for a different host or carries a marker such as @revoked, which is a line that exists precisely to say this key must not be accepted")
	default:
		out.WriteString("the key " + addr + " offers was refused by this backup set's known_hosts. Compare the offered fingerprint out of band before trusting it")
	}
	out.WriteString("\n  offered  " + fingerprint + " (" + algorithm + ")")
	if trustedErr != nil {
		out.WriteString("\n  trusted  unknown: this set's known_hosts could not be read to say what it does pin")
		return out.String()
	}
	if len(trusted) == 0 {
		out.WriteString("\n  trusted  nothing: this set's known_hosts holds no key at all")
		return out.String()
	}
	for _, k := range trusted {
		out.WriteString("\n  trusted  " + k.Fingerprint + " (" + k.Algorithm + ", line " + strconv.Itoa(k.Line) + ")")
	}
	return out.String()
}

// matchTrusted finds the offered key among the lines the file holds, for
// the DETAIL only. It decides nothing: Deps.Verify has already answered
// by the time either caller reaches this.
func matchTrusted(trusted []TrustedKey, offered string) (TrustedKey, bool) {
	for _, k := range trusted {
		if k.Fingerprint == offered {
			return k, true
		}
	}
	return TrustedKey{}, false
}

// resolveHost answers the resolve step, naming the record class beside
// each address the way `dig` does, because "it resolves to an IPv6
// address and your route to it is broken" is a real afternoon and an
// answer that said only "resolved" would hide it.
func resolveHost(ctx context.Context, host string) ([]string, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []string{host + " (an address already, so nothing was looked up)"}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, errors.New("sourcecheck: the resolver returned no addresses")
	}
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		class := "A"
		if a.IP.To4() == nil {
			class = "AAAA"
		}
		out = append(out, a.IP.String()+" ("+class+")")
	}
	return out, nil
}

// dial opens the connection StepConnect reports on and HANDS IT BACK
// open, because StepHostKey's key exchange runs over this same socket.
// Closing it here and dialling again would be a second connection, and
// then the key the operator is shown would not be the key that was
// offered on the connection that was measured.
func dial(ctx context.Context, addr string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// roundMillis renders a duration the way an operator reads one: whole
// milliseconds under a second, one decimal of a second above it. A
// connect reported as "41.283718ms" is a number nobody compares.
func roundMillis(d time.Duration) string {
	if d < time.Second {
		return strconv.FormatInt(d.Round(time.Millisecond).Milliseconds(), 10) + "ms"
	}
	return strconv.FormatFloat(d.Round(100*time.Millisecond).Seconds(), 'f', 1, 64) + "s"
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// dialProblem says why a connection attempt failed in the shapes an
// operator can act on, and never by echoing an error whose text may carry
// a resolver's internals or a path.
func dialProblem(err error) string {
	switch {
	case err == nil:
		return "no reason was reported"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "it did not answer within " + connectTimeout.String()
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "it did not answer within " + connectTimeout.String()
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return "the connection was refused or unreachable"
	}
	return "the connection could not be established"
}

// listProblem turns the adapter's own category for a failed listing into
// a sentence about the source, which is where this failure actually is.
func listProblem(category transport.Category, t Target, path string) string {
	switch category {
	case transport.NotFound:
		return "authentication succeeded and " + path + " is not there on that machine"
	case transport.PermissionDenied:
		return t.User + " authenticated and is not allowed to read " + path + ". That is a permissions problem on the source, and nothing to do with the key"
	case transport.Transient:
		return "authentication succeeded and the session dropped while listing " + path + "; the server is reachable and something interrupted it"
	case transport.UnsupportedCapability:
		return t.User + " authenticated and the server would not serve sftp for that account"
	default:
		return "authentication succeeded and " + path + " could not be listed. It may not exist on that machine, or " + t.User + " may not be allowed to read it"
	}
}

// readIdentification reads the server's SSH identification string and
// returns a net.Conn that replays it, so the handshake sees the stream
// exactly as it was sent.
//
// It reads at most one line, under a short deadline, and gives up
// silently: this is a nicety for the operator ("SSH-2.0-OpenSSH_9.6p1" is
// how you know you reached sshd and not a load balancer), never a
// verdict. Anything that goes wrong here leaves the banner empty and the
// stream untouched.
//
// The read deadline it sets is left on the socket rather than cleared on
// the way out. The key exchange that runs next sets its own the moment it
// starts, so the connection is never without one while it is in use,
// which is the property PR #628's review found missing: an earlier
// version cleared it here, and that was the last bound the handshake had.
func readIdentification(conn net.Conn) (string, net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))

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
	return printableIdentification(line), replay
}

// printableIdentification renders what the far end said so that printing
// it can do nothing but print.
//
// RFC 4253 restricts the identification string to printable US-ASCII,
// and until this existed nothing enforced it. The string is appended to
// the connect step's Detail, which reaches the API response, the obs
// event, and `activity --follow`'s stdout, which is usually a real TTY.
// So a server could answer with escape sequences and clear the scrollback
// an operator was reading as evidence, or scroll its own lines out of
// sight above the cursor: "SSH-2.0-OpenSSH_9.6p1\x1b[2J\x1b[H\a nothing
// to see here" did exactly that. The browser panel is not the exposure,
// because React escapes it; the terminal is. And an operator pointing a
// test connection at a host somebody else controls is not a stretch, it
// is what the candidate-mode wizard invites during setup.
//
// Escaped rather than dropped, and rather than refused. The banner is
// the one piece of evidence that says something answered SSH rather than
// merely accepting a socket, so an operator looking at a host that
// behaved like this needs to see what it sent, in a form that says so
// out loud. strconv.QuoteToASCII is exactly that rendering (control
// bytes as \x1b and \a, anything above 0x7e as \u, and backslashes and
// quotes escaped so the result is unambiguous) and it is byte-honest
// about invalid UTF-8, which a rune-by-rune mapping would quietly turn
// into replacement characters. Its surrounding quotes come off: this is
// appended into a sentence, not printed as a Go literal.
//
// Worst case is a 512-byte banner (maxIdentificationBytes) rendering as
// about 3 KB of \u escapes, which is bounded, inert, and what a server
// that sent 512 bytes of control codes deserves to look like.
func printableIdentification(line string) string {
	quoted := strconv.QuoteToASCII(line)
	return quoted[1 : len(quoted)-1]
}

// replayConn is a net.Conn whose reads start with bytes already taken off
// the wire. Everything else is the underlying connection's, including
// Close, so the deferred close in Run still closes the socket.
type replayConn struct {
	net.Conn
	pending io.Reader
}

func (c *replayConn) Read(p []byte) (int, error) { return c.pending.Read(p) }
