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
// # Three dials, and why that is the honest choice
//
// StepConnect dials TCP and closes. StepHostKey calls
// rclone.ProbeHostKey, which dials again and aborts the handshake the
// instant key exchange hands it the offered key. StepAuthenticate dials a
// third time, this time with credentials.
//
// Carrying one connection through all three would save two round trips
// against a host that is answering, and would mean reaching into a probe
// whose entire safety argument is that it never authenticates and holds
// no credentials. The second dial is cheap and the argument is not, so
// the dials are separate and the detail says so.
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
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// Step names one thing this check proves. The set is closed and ordered:
// Steps below is the order Run performs them in, and a Report always
// carries one Check per step so a surface renders a fixed list rather
// than discovering which ones happened to run.
type Step string

const (
	// StepCredentials is whether the private key this backup set names
	// can be obtained and opened on THIS host: read from its file, its
	// environment variable or its command, decrypted at rest if this
	// deployment encrypts keys at rest, and unlocked with the configured
	// passphrase if it has one. Nothing about the far side.
	StepCredentials Step = "credentials"

	// StepResolve is whether the hostname resolves, and to what. Its
	// detail names the addresses, because "it resolves" and "it resolves
	// to the machine you think" are different claims and only an
	// operator can settle the second.
	StepResolve Step = "resolve"

	// StepConnect is whether TCP to host:port answered, and how fast.
	// The timing is in the detail on purpose: a connect that succeeds in
	// four seconds is a different report from one that succeeds in
	// twelve milliseconds, and neither is a failure.
	StepConnect Step = "connect"

	// StepHostKey is which key the server offered, its SHA256
	// fingerprint, and whether it matches the line this backup set
	// trusts. Both fingerprints go in the detail in `ssh-keygen -lf`
	// form, so an operator can compare them by eye against an
	// out-of-band source, which is the only thing that actually settles
	// a mismatch.
	StepHostKey Step = "host_key"

	// StepAuthenticate is whether the server accepted that key for that
	// user. It is skipped when the host key did not match: offering a
	// credential to a server whose identity has not been established is
	// the thing FR-6 exists to prevent.
	StepAuthenticate Step = "authenticate"

	// StepList is the configured remote path listed over the real
	// transport, and how many entries came back. It is the only step
	// that proves the path in the configuration is a path on that
	// machine.
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
	// Credentials resolves the private key this backup set names into a
	// signer, through the same key/passphrase/at-rest-encryption
	// machinery a real transfer uses. It returns the signer's public
	// half's SHA256 fingerprint too, which is what an operator compares
	// against the authorized_keys line on the far side.
	Credentials func(context.Context) (signer ssh.Signer, fingerprint string, err error)

	// Trusted reads the host keys this backup set's own known_hosts file
	// pins for this host:port.
	Trusted func(context.Context) ([]TrustedKey, error)

	// ProbeHostKey captures the key the server offers, without
	// authenticating. rclone.ProbeHostKey in production.
	ProbeHostKey func(ctx context.Context, host string, port int) (algorithm, fingerprint string, err error)

	// List lists the configured remote path over the real transport and
	// reports how many entries came back.
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

// authTimeout bounds the SSH handshake and authentication in
// StepAuthenticate. Longer than the TCP dial because it covers a key
// exchange and a signature on both ends.
const authTimeout = 10 * time.Second

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

	signer, keyFingerprint, err := deps.Credentials(ctx)
	if err != nil {
		deps.observe(StepCredentials, err)
		b.fail(StepCredentials, CategoryCredentials, credentialsFailureDetail(target))
		b.skipRest(StepResolve, "the key this backup set names could not be opened on this host, so nothing was tried against "+target.Host)
		return b.report()
	}
	b.pass(StepCredentials, credentialsPassedDetail(target, keyFingerprint))

	addresses, err := resolveHost(ctx, target.Host)
	if err != nil {
		deps.observe(StepResolve, err)
		b.fail(StepResolve, CategoryDNS, "this host does not resolve to an address from this machine: "+target.Host)
		b.skipRest(StepConnect, "the hostname did not resolve, so there was no address to connect to")
		return b.report()
	}
	b.pass(StepResolve, target.Host+" is "+strings.Join(addresses, " and "))

	addr := net.JoinHostPort(target.Host, strconv.Itoa(target.Port))
	elapsed, err := dial(ctx, addr)
	if err != nil {
		deps.observe(StepConnect, err)
		b.fail(StepConnect, CategoryNetwork, "nothing answered TCP on "+addr+" within "+connectTimeout.String())
		b.skipRest(StepHostKey, "nothing answered on "+addr+", so no host key was offered")
		return b.report()
	}
	b.pass(StepConnect, "TCP to "+addr+" in "+roundMillis(elapsed))

	algorithm, offered, err := deps.ProbeHostKey(ctx, target.Host, target.Port)
	if err != nil {
		deps.observe(StepHostKey, err)
		b.fail(StepHostKey, CategoryNetwork, addr+" answered TCP but did not complete an SSH key exchange, so it is not offering a host key this manager can read")
		b.skipRest(StepAuthenticate, "no host key was read, so nothing was offered to this server")
		return b.report()
	}

	trusted, err := deps.Trusted(ctx)
	if err != nil {
		deps.observe(StepHostKey, err)
		b.fail(StepHostKey, CategoryHostKey, "this backup set's known_hosts file could not be read, so the key "+addr+" offered could not be compared against anything")
		b.skipRest(StepAuthenticate, "the offered host key was not compared, so nothing was offered to this server")
		return b.report()
	}

	match, ok := matchTrusted(trusted, offered)
	if !ok {
		deps.observe(StepHostKey, fmt.Errorf("sourcecheck: %s offered %s %s and this set trusts %d other key(s)", addr, algorithm, offered, len(trusted)))
		b.fail(StepHostKey, CategoryHostKey, hostKeyMismatchDetail(algorithm, offered, trusted))
		b.skipRest(StepAuthenticate, "the host key did not match, so nothing was offered to this server")
		return b.report()
	}
	b.pass(StepHostKey, hostKeyMatchDetail(algorithm, offered, match))

	if err := authenticate(ctx, addr, target.User, signer, offered); err != nil {
		deps.observe(StepAuthenticate, err)
		b.fail(StepAuthenticate, CategoryAuthentication, "the server did not accept this key for "+target.User+"@"+target.Host+". On that machine, the public half has to be in "+target.User+"'s authorized_keys")
		b.skipRest(StepList, "authentication did not succeed, so the remote path was never listed")
		return b.report()
	}
	b.pass(StepAuthenticate, "the server accepted publickey for "+target.User)

	entries, err := deps.List(ctx)
	if err != nil {
		deps.observe(StepList, err)
		b.fail(StepList, CategoryRemotePath, "authentication succeeded and "+remotePathOf(target)+" could not be listed. It may not exist on that machine, or "+target.User+" may not be allowed to read it")
		return b.report()
	}
	b.pass(StepList, remotePathOf(target)+" listed, "+plural(entries, "entry", "entries"))

	return b.report()
}

// remotePathOf is the path this set is configured to read, with the same
// default the service applies when a set names none.
func remotePathOf(t Target) string {
	if t.RemotePath == "" {
		return "/"
	}
	return t.RemotePath
}

func credentialsPassedDetail(t Target, fingerprint string) string {
	out := "read the private key " + keyReferenceOf(t)
	if t.PassphraseConfigured {
		out += " and opened it with the configured passphrase"
	}
	if fingerprint != "" {
		out += "; its public half is " + fingerprint
	}
	return out
}

func credentialsFailureDetail(t Target) string {
	if t.PassphraseConfigured {
		return "the private key " + keyReferenceOf(t) + " could not be read on this host, or the configured passphrase does not open it"
	}
	return "the private key " + keyReferenceOf(t) + " could not be read on this host"
}

func keyReferenceOf(t Target) string {
	if t.KeyReference == "" {
		return "this backup set is configured with"
	}
	return t.KeyReference
}

// hostKeyMatchDetail is the good news, and it still prints both
// fingerprints rather than the word "matched". An operator reading a log
// after a host was rebuilt wants to see WHICH key was accepted, and a
// panel that only ever says "matched" has nothing to show them.
func hostKeyMatchDetail(algorithm, offered string, match TrustedKey) string {
	return "the server offered " + algorithm + " " + offered +
		", matching the key this backup set trusts on line " + strconv.Itoa(match.Line) + " of its known_hosts"
}

func hostKeyMismatchDetail(algorithm, offered string, trusted []TrustedKey) string {
	var out strings.Builder
	out.WriteString("the key this server offers is not the one this backup set trusts. Compare the offered fingerprint out of band before trusting it")
	out.WriteString("\n  offered  " + offered + " (" + algorithm + ")")
	if len(trusted) == 0 {
		out.WriteString("\n  trusted  nothing: this set's known_hosts holds no key for this host")
		return out.String()
	}
	for _, k := range trusted {
		out.WriteString("\n  trusted  " + k.Fingerprint + " (" + k.Algorithm + ", line " + strconv.Itoa(k.Line) + ")")
	}
	return out.String()
}

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

func dial(ctx context.Context, addr string) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	var dialer net.Dialer
	start := time.Now()
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	elapsed := time.Since(start)
	if err != nil {
		return 0, err
	}
	_ = conn.Close()
	return elapsed, nil
}

// authenticate proves the server accepts this key for this user, and
// nothing else. It opens no channel, runs no command and reads no path:
// that is StepList's job over the real transport.
//
// The host key callback pins the exact fingerprint StepHostKey already
// established and matched, rather than reading known_hosts again. Two
// reasons: this dial must not be able to succeed against a DIFFERENT key
// than the one just reported to the operator, and re-reading the file
// would make a passing authenticate step depend on a parse this package
// has already done once.
func authenticate(ctx context.Context, addr, user string, signer ssh.Signer, fingerprint string) error {
	ctx, cancel := context.WithTimeout(ctx, authTimeout)
	defer cancel()

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	cfg := &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			if ssh.FingerprintSHA256(key) != fingerprint {
				return errors.New("sourcecheck: the server offered a different host key than the one just probed")
			}
			return nil
		},
		Timeout: authTimeout,
	}

	client, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		return err
	}
	go ssh.DiscardRequests(reqs)
	go func() {
		for ch := range chans {
			_ = ch.Reject(ssh.Prohibited, "this connection test opens no channels")
		}
	}()
	return client.Close()
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
