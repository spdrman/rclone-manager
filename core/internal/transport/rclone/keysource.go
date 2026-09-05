// This file is #74's other two key sources: an environment variable, and an
// external command. ssh.go's sftpConfig calls into exactly one of
// resolveKeyFromEnv or resolveKeyFromCommand, only when the source did not
// choose key_file, and only ever hands the result to rclone's key_pem
// option, never to anything this program logs or returns to a caller in
// the clear.
//
// # Why this exists at all, and why key_file does not go through it
//
// key_file's whole security property is that the key never enters this
// process's memory: rclone opens the file itself. The moment a resolver's
// output has to be typed somewhere other than a filesystem path, that
// property is gone by construction, so this file exists to make everything
// downstream of that moment as narrow and as defensible as it can be:
//
//   - the resolved bytes are validated as an unencrypted SSH private key
//     BEFORE anything wraps them in obs.Secret or hands them to rclone. A
//     secrets manager that answers with an error string, an HTML login
//     page, or an empty body on a failed auth fails loudly here, at the
//     point the bytes were produced, instead of surfacing as a confusing
//     rclone dial failure far away from the actual cause.
//   - a passphrase-protected key is refused by name rather than handed to
//     rclone to fail on (or, worse, hang on) later. There is nowhere in
//     this program's unattended operation for a passphrase prompt to go.
//   - resolved stdout is never, under any circumstance, echoed back in an
//     error message. A failure to parse is reported by the shape of the
//     problem (empty, encrypted, not a key at all), never by the content
//     that failed, precisely because that content might itself be a
//     partially-correct secret. Compare this to a command's stderr, which
//     is diagnostic text by convention (the same distinction
//     internal/lifecycle/verify.go's external validator draws between its
//     exit code and its captured output) and is safe, and useful, to
//     surface to an operator debugging a broken resolver.
//   - a command resolver's argv is run directly, never through a shell.
//     exec.CommandContext execs argv[0] with the rest of argv as literal
//     arguments; there is no shell anywhere in this call for a
//     metacharacter in any element to mean anything to. It also runs with
//     a bounded timeout, a fixed minimal environment, and in its own
//     process group so the timeout can kill anything it spawned along the
//     way, on the same reasoning internal/lifecycle/verify.go already
//     applies to its own external, untrusted validator.
//   - whatever memory this file owns outright (the subprocess's captured
//     stdout, an environment value's own []byte copy) is overwritten
//     before it is released. What it does NOT claim to zero is the string
//     that becomes obs.Secret's payload, or the process's own copy of its
//     environment block: Go gives no supported way to mutate either
//     without corrupting shared state, and pretending otherwise would be a
//     less honest kind of undefended than just saying so.
package rclone

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/spdrman/rclone-manager/core/internal/obs"
)

// keyCommandTimeout bounds how long a `command` key resolver is allowed to
// run before it is killed. A secrets-manager CLI call is expected to be a
// single round trip (read one secret, print it, exit), not a long-running
// operation, so this is generous for that shape of work without leaving a
// misbehaving resolver free to hang a connection attempt indefinitely.
//
// This is a fixed constant in production, not a config field, on purpose:
// #74's proposed shape has no timeout knob for `command`, and adding one
// here would be scope this issue does not ask for. A resolver command's own
// context.Background()-rooted deadline (rather than one derived from a
// caller's ctx) is the same trade-off; see resolveKeyFromCommand's doc for
// why. It is a var, not a const, only so keysource_test.go can shorten it
// for TestResolveKeyFromCommand_Timeout without an actual 15-second test.
var keyCommandTimeout = 15 * time.Second

// resolverReapBackstop is os/exec's own backstop, set as c.WaitDelay on
// every resolver subprocess in this package: that long after the context
// is done, os/exec kills the child and stops waiting for its pipes to
// drain, whatever c.Cancel did or failed to do.
//
// It is named, and read by the test, because it is the SECOND of the two
// regimes a timeout test has to tell apart, and the one that makes such a
// test vacuous if a bound is drawn above it (#394). A resolver whose
// c.Cancel really kills the process group returns at about
// keyCommandTimeout. One whose c.Cancel does nothing returns at
// keyCommandTimeout + resolverReapBackstop, because this is what finally
// reaps it, and every other assertion in that test passes either way. So
// the bound has to sit strictly between the two, which it cannot do
// reliably if the test has to guess this number.
//
// internal/lifecycle/verify_test.go's hookReapBackstop mirrors the same
// value as a test-side constant and says in its own doc that nothing reads
// it from the production code. This is that gap closed on this side.
const resolverReapBackstop = 5 * time.Second

// maxResolvedKeySize bounds how many bytes of a command's stdout, or an
// environment variable's value, this resolver will accept. The largest
// private key this project has any real reason to see (an RSA-4096 PEM) is
// a few kilobytes; this is a wide margin meant to stop a misbehaving or
// compromised resolver from exhausting memory, not a realistic key-size
// ceiling.
const maxResolvedKeySize = 1 << 20 // 1 MiB

// maxCapturedStderr bounds how much of a failing key command's stderr this
// resolver keeps for its error message. Stderr is diagnostic text, not
// secret-shaped, so it is safe to surface, but an unbounded capture would
// let a runaway process bloat an error message (or exhaust memory) just as
// easily as an unbounded stdout would.
const maxCapturedStderr = 4 << 10 // 4 KiB

// resolveKeyFromEnv reads name from the environment and returns it wrapped
// in an obs.Secret, once it has been confirmed to parse as an SSH private
// key. passphrase is "" for an unencrypted key (the common case, and every
// call site before #269); when non-empty, it is what the resolved key
// material is expected to decrypt with, checked here rather than left for
// rclone to discover far away from the actual cause.
func resolveKeyFromEnv(name, passphrase string) (obs.Secret, error) {
	val, ok := os.LookupEnv(name)
	if !ok {
		return obs.Secret{}, fmt.Errorf("environment variable %q is not set", name)
	}

	// []byte(val) copies: val itself is backed by Go's own copy of the
	// process environment block, shared with every other lookup of the
	// same variable, and there is no supported way to zero that without
	// corrupting it for whoever reads it next. buf is a copy this function
	// owns outright, so it is what gets overwritten below.
	buf := []byte(val)
	defer zeroBytes(buf)

	secret, err := validateAndWrapKey(buf, passphrase)
	if err != nil {
		return obs.Secret{}, fmt.Errorf("environment variable %q: %w", name, err)
	}
	return secret, nil
}

// resolveKeyFromCommand runs argv and treats its stdout as SSH private key
// material, once that has been confirmed to parse as an unencrypted SSH
// private key.
//
// The exec discipline it runs under (never a shell, a bounded timeout, a
// fixed minimal environment, its own process group, bounded and zeroed
// stdout, stderr surfaced and stdout never) lives in runResolverCommand
// below, which mediumcreds.go's own command resolver calls too. It is the
// same code this function always ran; #235 lifted it so the two resolvers
// cannot drift apart.
func resolveKeyFromCommand(argv []string, passphrase string) (obs.Secret, error) {
	stdout, err := runResolverCommand(argv, maxResolvedKeySize)
	if stdout != nil {
		defer stdout.zero() // whatever happens, never leave resolved bytes sitting in this buffer
	}
	if err != nil {
		return obs.Secret{}, err
	}

	secret, err := validateAndWrapKey(stdout.buf.Bytes(), passphrase)
	if err != nil {
		return obs.Secret{}, fmt.Errorf("key command %q: %w", argv[0], err)
	}
	return secret, nil
}

// runResolverCommand runs argv and returns its captured stdout, under the
// custody discipline every secret resolver in this package shares.
//
// It exists as one function, rather than once per resolver, because every
// clause below is a hardening decision, and a hardening that lives in one
// resolver and not the other is a hardening this package does not actually
// have. #235 added the second caller (medium credentials, mediumcreds.go);
// this is the same code resolveKeyFromCommand always ran, lifted rather
// than copied.
//
//   - argv[0] is the executable; the rest are its literal arguments.
//     exec.CommandContext never invokes a shell, so a shell metacharacter
//     anywhere in argv (";", "|", "$(...)", backticks, "&&", a trailing
//     "&", ...) is inert: it reaches argv[0] as one literal byte sequence
//     inside a single argument, never as a second command, a substitution
//     or a pipeline.
//   - a fixed, minimal environment, never this process's own os.Environ():
//     the resolver command is meant to be self-sufficient, not handed
//     ambient state this manager holds for something unrelated. Same
//     reasoning internal/lifecycle/verify.go applies to its own external
//     validator.
//   - its own process group, so the timeout can kill whatever it spawned
//     along with it.
//   - stdout is bounded and is NEVER surfaced in an error, whatever
//     happened to it. Stderr is diagnostic text by convention, is bounded
//     separately, and IS surfaced, because an operator debugging a broken
//     resolver needs it.
//
// The timeout is rooted in context.Background(), not a caller's ctx, for
// the reason resolveKeyFromCommand's doc has always given: the only call
// sites are configuration-building functions that carry no context of
// their own, and this resolver's own fixed timeout already bounds how long
// it can run.
//
// The returned buffer is non-nil whenever a command actually ran, even on
// failure, so the caller can zero it; callers do that with a deferred
// stdout.zero().
func runResolverCommand(argv []string, limit int) (*boundedBuffer, error) {
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return nil, errors.New("no executable configured")
	}

	ctx, cancel := context.WithTimeout(context.Background(), keyCommandTimeout)
	defer cancel()

	c := exec.CommandContext(ctx, argv[0], argv[1:]...)

	// A fixed, minimal environment, never this process's own os.Environ():
	// the resolver command is meant to be self-sufficient (an absolute
	// executable path, any auth it needs already configured on disk or
	// baked into a wrapper script), not handed ambient state this manager
	// holds for something unrelated. Same reasoning
	// internal/lifecycle/verify.go's runValidator already applies to its
	// own external, untrusted validator.
	c.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin"}

	// Its own process group, so the timeout below can kill whatever it
	// spawned along with it, mirroring runValidator's identical setup.
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error {
		if c.Process == nil {
			return nil
		}
		if err := syscall.Kill(-c.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		return nil
	}
	c.WaitDelay = resolverReapBackstop

	stdout := &boundedBuffer{limit: limit}
	stderr := &boundedBuffer{limit: maxCapturedStderr}
	c.Stdout = stdout
	c.Stderr = stderr
	runErr := c.Run()

	switch {
	case ctx.Err() != nil:
		return stdout, fmt.Errorf("command %q: killed after exceeding its %s timeout", argv[0], keyCommandTimeout)
	case runErr != nil:
		// Infrastructure failure (bad executable, non-zero exit, ...): the
		// command's own diagnosis of what went wrong, not its stdout,
		// which is never surfaced here regardless of whether it happened
		// to contain anything sensitive.
		return stdout, fmt.Errorf("command %q: %v (stderr: %s)", argv[0], runErr, stderr.String())
	case stdout.truncated:
		return stdout, fmt.Errorf("command %q: output exceeded %d bytes, refusing to treat truncated output as secret material", argv[0], limit)
	}

	return stdout, nil
}

// validateAndWrapKey is the one place raw resolver output is checked and, if
// it passes, wrapped in an obs.Secret. Both resolvers above, and
// ValidateImportedPrivateKey below, call this and only this to turn bytes
// into a value the rest of ssh.go is allowed to touch.
//
// passphrase is "" for the unencrypted case, #74's original and still the
// documented preference: raw is parsed exactly as before #269, and an
// encrypted key is refused by name rather than handed to rclone to fail
// (or hang) on. When passphrase is non-empty (#269), raw is instead
// required to decrypt with exactly that passphrase; a key that does not
// need one, or does not decrypt with the one given, is refused just as
// clearly. Either way this is a config-time check, made once here, rather
// than a question rclone answers for the first time when a cycle actually
// tries to connect.
//
// raw is never included in a returned error, on purpose: whether it is
// empty, an HTML login page, an error string from a secrets manager, or a
// genuinely encrypted key, the failure is reported by naming what kind of
// problem it is, not by echoing the bytes that had the problem.
func validateAndWrapKey(raw []byte, passphrase string) (obs.Secret, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return obs.Secret{}, errors.New("resolved key material is empty")
	}

	if passphrase != "" {
		if _, err := ssh.ParseRawPrivateKeyWithPassphrase(raw, []byte(passphrase)); err != nil {
			if errors.Is(err, x509.IncorrectPasswordError) {
				return obs.Secret{}, errors.New("resolved key did not decrypt with the configured passphrase")
			}
			// Same "report the shape of the problem, never the bytes" rule
			// as the unencrypted branch below: a key that turns out not to
			// be encrypted at all, one in a format this program does not
			// support, or genuinely corrupt data, all land here.
			return obs.Secret{}, fmt.Errorf("resolved key material does not parse as a valid SSH private key with the configured passphrase: %v", err)
		}
		return obs.NewSecret(string(raw)), nil
	}

	if _, err := ssh.ParseRawPrivateKey(raw); err != nil {
		var passphraseErr *ssh.PassphraseMissingError
		if errors.As(err, &passphraseErr) {
			return obs.Secret{}, errors.New("resolved key is passphrase-protected; configure key.passphrase (file, env or command) to supply it")
		}
		// Any other parse failure (no PEM block at all, an unsupported key
		// type, truncated/corrupt data, ...) is reported the same way,
		// generically: a secrets manager returning an error string, an
		// HTML login page, or garbage all land here, and all of them mean
		// the same thing to this adapter, "this is not a private key",
		// regardless of which specific reason x/crypto/ssh gives.
		return obs.Secret{}, fmt.Errorf("resolved key material does not parse as a valid SSH private key: %v", err)
	}

	return obs.NewSecret(string(raw)), nil
}

// ValidateImportedPrivateKey checks raw the same way validateAndWrapKey
// checks a key.env/key.command resolver's output (must parse as an SSH
// private key, with passphrase exactly as validateAndWrapKey's own doc
// describes, never echoed back on failure), and, on success, also reports
// the key's algorithm and SHA256 fingerprint in the same "SHA256:base64…"
// form `ssh-keygen -lf` prints and FingerprintDisplay.tsx already renders.
//
// This is issue #146 (B2.7)'s SSH-key-import API surface (POST /ssh-keys)
// reusing this file's existing validation rather than a second, parallel
// implementation of "is this a usable SSH private key" at the HTTP layer:
// core/service calls this directly (core/service is inside core/'s own
// module tree, same as every other internal/ caller), and never re-derives
// the parse/fingerprint logic itself. Reusing validateAndWrapKey here is
// also what #269's acceptance criteria asks for directly: POST /ssh-keys
// and a key.file/key.passphrase configuration share this one function's
// verdict on what decrypts and what does not, so the two can never
// disagree about whether a given key and passphrase actually work
// together.
//
// The fingerprint is computed with ssh.ParsePrivateKey/
// ParsePrivateKeyWithPassphrase rather than the ssh.ParseRawPrivateKey*
// family validateAndWrapKey already called, because only the former
// returns an ssh.Signer with a PublicKey() to fingerprint; both calls
// succeed or fail together for the same input plus passphrase, so this
// function never wraps or returns anything from a validateAndWrapKey
// failure.
func ValidateImportedPrivateKey(raw []byte, passphrase string) (secret obs.Secret, algorithm, fingerprint string, err error) {
	secret, err = validateAndWrapKey(raw, passphrase)
	if err != nil {
		return obs.Secret{}, "", "", err
	}
	var signer ssh.Signer
	var parseErr error
	if passphrase != "" {
		signer, parseErr = ssh.ParsePrivateKeyWithPassphrase(raw, []byte(passphrase))
	} else {
		signer, parseErr = ssh.ParsePrivateKey(raw)
	}
	if parseErr != nil {
		// Same "report the shape of the problem, never the bytes" rule
		// validateAndWrapKey's own doc states: raw is never included here.
		return obs.Secret{}, "", "", fmt.Errorf("resolved key material does not parse as a valid SSH private key: %v", parseErr)
	}
	return secret, signer.PublicKey().Type(), ssh.FingerprintSHA256(signer.PublicKey()), nil
}

// zeroBytes overwrites b in place. runtime.KeepAlive after the loop is
// there so the compiler cannot prove the writes are dead and elide them,
// which is as far as "zeroed... where Go allows" reaches: Go has no
// memory-locking or guaranteed-erase primitive, so this defends against an
// accidental later reuse of freed memory (a heap dump, a debugger
// inspecting a stale allocation), not against an attacker who already has
// full access to this process's live memory.
func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
	runtime.KeepAlive(b)
}

// boundedBuffer caps how many bytes of a subprocess stream it retains,
// mirroring internal/lifecycle/verify.go's boundedWriter and its reasoning
// (an untrusted process must never be able to block by writing past a
// limit, nor exhaust memory by writing without one). It is redefined here,
// rather than exported from lifecycle, because internal/lifecycle is out of
// this change's scope, and because transport importing lifecycle would
// invert this project's dependency direction: lifecycle depends on
// transport, never the reverse.
type boundedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

// Write keeps at most limit bytes and reports len(p) regardless, which is
// the whole point. An io.Writer that returned a short count would make
// os/exec's copier treat the truncation as a write error and kill the
// pipe, so a resolver that printed too much would look like a resolver
// that crashed. Instead everything past the limit is dropped, truncated
// records that it happened, and the caller decides: runResolverCommand
// refuses to treat truncated output as secret material rather than
// silently using a prefix of a key.
func (w *boundedBuffer) Write(p []byte) (int, error) {
	room := w.limit - w.buf.Len()
	switch {
	case room <= 0:
		if len(p) > 0 {
			w.truncated = true
		}
	case room < len(p):
		w.buf.Write(p[:room])
		w.truncated = true
	default:
		w.buf.Write(p)
	}
	return len(p), nil
}

// String returns what was retained. Only ever called on the stderr buffer:
// stdout's content is secret material and is never rendered into an error,
// which is the distinction this whole file is built around.
func (w *boundedBuffer) String() string { return w.buf.String() }

// zero overwrites this buffer's retained bytes in place. Meaningful only
// for a buffer that might hold key material (stdout); stderr is diagnostic
// text, never a secret, and callers do not zero it.
func (w *boundedBuffer) zero() {
	zeroBytes(w.buf.Bytes())
}
