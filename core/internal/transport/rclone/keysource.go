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

// resolverCommandTimeout bounds how long a `command` resolver is allowed to
// run before it is killed, whether it is resolving an SSH key (#74) or a
// storage medium's S3 credentials (#235, FR-33). A secrets-manager CLI call
// is expected to be a single round trip (read one secret, print it, exit),
// not a long-running operation, so this is generous for that shape of work
// without leaving a misbehaving resolver free to hang a connection attempt
// indefinitely.
//
// One constant for both because it is one kind of work with one shape, and
// two knobs that always want the same value are two things to keep in sync.
//
// This is a fixed constant in production, not a config field, on purpose:
// #74's proposed shape has no timeout knob for `command`, and adding one
// here would be scope neither issue asks for. A resolver command's own
// context.Background()-rooted deadline (rather than one derived from a
// caller's ctx) is the same trade-off; see runResolverCommand's doc for
// why. It is a var, not a const, only so tests can shorten it without an
// actual 15-second test.
var resolverCommandTimeout = 15 * time.Second

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
// argv[0] is the executable; the rest are its literal arguments.
// exec.CommandContext never invokes a shell, so a shell metacharacter
// anywhere in argv (";", "|", "$(...)", backticks, "&&", a trailing "&", ...)
// is inert: it is passed to argv[0] as one literal byte sequence inside a
// single argument, exactly like every other byte in that argument, never
// interpreted as a second command, a substitution or a pipeline.
//
// The timeout below is rooted in context.Background(), not a ctx this
// function's caller could supply. sftpConfig, the only caller, does not
// carry a context of its own today (see adapter.go's fsFor, which owns the
// context this whole call chain is running under but does not thread it
// down this far), and adding one is a change to the shape of an existing,
// well-tested function that this issue's scope does not require: this
// resolver's own fixed timeout already bounds how long it can run,
// independent of whatever the caller's context does.
func resolveKeyFromCommand(argv []string, passphrase string) (obs.Secret, error) {
	stdout, done, err := runResolverCommand(argv, "key command", "key material", maxResolvedKeySize)
	if err != nil {
		return obs.Secret{}, err
	}
	defer done()

	secret, verr := validateAndWrapKey(stdout, passphrase)
	if verr != nil {
		return obs.Secret{}, fmt.Errorf("key command %q: %w", argv[0], verr)
	}
	return secret, nil
}

// runResolverCommand runs argv under the discipline this file's package doc
// describes and returns whatever it printed on stdout, for a caller to
// validate by shape.
//
// It exists because #235 needed the identical discipline for a storage
// medium's S3 credentials, and the honest way to reuse a security control
// is to have one copy of it rather than a second one that starts out
// identical. Everything the SSH key resolver relied on is here and applies
// to both callers unchanged: argv is exec'd directly so no shell exists for
// a metacharacter to mean anything to, the environment is fixed and minimal
// rather than this process's own, the child gets its own process group so
// the timeout can kill whatever it spawned, stdout and stderr are both
// bounded, and stdout is zeroed on every path out.
//
// what names the resolver in error messages ("key command", "credentials
// command") and material names what its output was meant to be ("key
// material", "credential material"), so a failure reads as a sentence about
// the thing that actually failed.
//
// The returned done() zeroes the captured stdout. A caller must defer it
// immediately, and must not retain the returned slice past that call: the
// bytes it points at are the ones done() overwrites.
//
// stdout is NEVER included in a returned error, on any path. Stderr is,
// because stderr is diagnostic text by convention and is the only thing an
// operator debugging a broken resolver has to go on. That distinction is
// the same one internal/lifecycle/verify.go's external validator draws
// between its exit code and its captured output.
//
// The deadline is rooted in context.Background() rather than a caller's
// context, because neither caller has one to give: sftpConfig and s3Config
// are both called from a place that does not thread a context down this far
// (see adapter.go's fsFor, which owns the context this call chain runs under
// but does not pass it), and this resolver's own fixed timeout already
// bounds how long it can run regardless of what the caller's context does.
func runResolverCommand(argv []string, what, material string, limit int) (stdout []byte, done func(), err error) {
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return nil, func() {}, fmt.Errorf("%s: no executable configured", what)
	}

	ctx, cancel := context.WithTimeout(context.Background(), resolverCommandTimeout)
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
	c.WaitDelay = 5 * time.Second

	out := &boundedBuffer{limit: limit}
	errOut := &boundedBuffer{limit: maxCapturedStderr}
	c.Stdout = out
	c.Stderr = errOut

	runErr := c.Run()

	switch {
	case ctx.Err() != nil:
		out.zero()
		return nil, func() {}, fmt.Errorf("%s %q: killed after exceeding its %s timeout", what, argv[0], resolverCommandTimeout)
	case runErr != nil:
		// Infrastructure failure (bad executable, non-zero exit, ...): the
		// command's own diagnosis of what went wrong, not its stdout,
		// which is never surfaced here regardless of whether it happened
		// to contain anything sensitive.
		out.zero()
		return nil, func() {}, fmt.Errorf("%s %q: %v (stderr: %s)", what, argv[0], runErr, errOut.String())
	case out.truncated:
		out.zero()
		return nil, func() {}, fmt.Errorf("%s %q: output exceeded %d bytes, refusing to treat truncated output as %s", what, argv[0], limit, material)
	}

	return out.buf.Bytes(), out.zero, nil
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

func (w *boundedBuffer) String() string { return w.buf.String() }

// zero overwrites this buffer's retained bytes in place. Meaningful only
// for a buffer that might hold key material (stdout); stderr is diagnostic
// text, never a secret, and callers do not zero it.
func (w *boundedBuffer) zero() {
	zeroBytes(w.buf.Bytes())
}
