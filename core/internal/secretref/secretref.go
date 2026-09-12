// Package secretref resolves a secret this program is told WHERE to find,
// into memory, for as short a time as the caller can arrange.
//
// # Why this exists
//
// The embedded backup engine's repositories are encrypted, and the two
// things needed to reach one -- the repository passphrase and, for an S3
// repository, the object-store credentials -- have to be Go strings inside
// this process. Kopia's native filesystem and S3 providers take them as
// parameters; there is no "hand the credential file to the backend and let
// it open it" option of the kind internal/transport/rclone prefers for the
// medium plane, because there is no separate process to hand it to.
//
// So the repository plane needs what the medium plane deliberately avoids:
// an in-process resolver. This package is that resolver, and nothing else.
// It holds no state, caches nothing, and returns obs.Secret so that the
// value it produces cannot be logged, formatted or serialised by accident.
//
// # This is not a second place to declare a secret
//
// There is exactly one shape in which an operator names a secret in this
// product's configuration -- file, env or command, never the material
// itself -- and Ref is that shape, not a variant of it.
// config.Passphrase, config.MediumCredentials and
// transport.MediumCredentials are the same three fields, and
// TestRefFieldSetMatchesTheConfiguredOnes fails if any of them grows a
// fourth, because a source an operator can write and this package cannot
// resolve is the exact failure that makes "one custody model" stop being
// true.
//
// What is genuinely duplicated is the MECHANISM: internal/transport/rclone
// resolves its own copies of the same three sources, privately, in a shape
// built around rclone's configmap. Unifying the two is a refactor of that
// package's most security-sensitive file and it is tracked separately; it
// is not something to attempt sideways from the repository adapter. The
// duplication that matters -- a second thing an operator has to configure,
// or a second set of rules about what is acceptable material -- is what
// the pinning test above forbids.
//
// # What a resolved secret is allowed to be
//
// Resolve returns one opaque value with trailing whitespace removed and
// refuses an empty answer, because a passphrase with a stray newline on
// the end and a passphrase that resolved to nothing both surface far away
// from here as "incorrect repository passphrase", which sends an operator
// looking for a lost key instead of a broken resolver.
//
// ResolveAWS returns an access key set, and it validates the SHAPE of what
// came back before anything uses it: a secrets manager answering with an
// error string, an HTML login page or a truncated file fails here, at the
// point the bytes were produced, rather than later as an unattributable
// AccessDenied.
//
// # What never happens
//
// No resolved material is ever rendered into an error, a log line or a
// String method, whatever went wrong. Errors name the SOURCE (which file,
// which variable, which executable) because that is what an operator has
// to go and fix, and they name nothing else. Buffers holding material are
// zeroed as soon as the value has been wrapped, as far as Go allows.
package secretref

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/backupdproject/backupd/core/internal/obs"
)

// maxSecretSize bounds how many bytes this package will accept as secret
// material from any source.
//
// A repository passphrase is tens of bytes and a shared-credentials file
// with one profile is a few hundred; 64 KiB is a wide margin meant to stop
// a misbehaving or compromised resolver from exhausting memory, not a
// realistic ceiling. The bound is enforced rather than warned about: a
// truncated secret is not a secret, and using a prefix of one is how a
// repository ends up with a passphrase nobody can reproduce.
const maxSecretSize = 64 << 10

// commandTimeout bounds how long a `command` resolver may run.
//
// It is generous because a real secrets-manager client authenticates,
// makes a network call and may retry, and it is bounded because this runs
// on the path that opens a repository: a resolver that hangs would turn a
// backup window into an indefinite stall rather than a failed run somebody
// can see.
var commandTimeout = 15 * time.Second

// reapBackstop is how long exec waits after the kill signal before giving
// up on the child's pipes, so a command that ignores SIGKILL on a stuck
// syscall cannot hold this process open forever.
const reapBackstop = 5 * time.Second

// maxCapturedStderr bounds how much of a failing resolver's stderr is kept
// for the error message. Stderr is diagnostic text by convention and IS
// surfaced, because an operator debugging a broken resolver needs it;
// stdout is material and is never surfaced.
const maxCapturedStderr = 4 << 10

// ErrNoSource is returned when a reference names no source at all.
//
// It is distinct from every other refusal because it is the one that is
// sometimes not an error at the CALLER's level: a backup set running the
// artifact engine has no repository and therefore no passphrase, and the
// caller decides whether silence is acceptable. This package never guesses
// on its behalf.
var ErrNoSource = errors.New("secretref: no secret source is declared")

// ErrAmbiguousSource is returned when a reference names more than one
// source. Choosing one would mean an operator who added an env var while
// leaving a stale file path behind gets whichever this package happened to
// check first, silently, for as long as both keep working.
var ErrAmbiguousSource = errors.New("secretref: more than one secret source is declared, and there is no rule for choosing between them")

// ErrEmpty is returned when a source resolved to nothing, or to whitespace
// only. See the package doc: an empty passphrase is reported downstream as
// a wrong one.
var ErrEmpty = errors.New("secretref: the declared source resolved to no material")

// ErrTooLarge is returned when a source produced more than maxSecretSize
// bytes. The refusal is deliberate rather than a truncation.
var ErrTooLarge = errors.New("secretref: the declared source produced more material than a secret can be")

// Ref names WHERE a secret comes from, and carries no material.
//
// Exactly one of the three may be set. It is the runtime form of
// config.Passphrase and config.MediumCredentials, field for field, and a
// test in this package pins that: see the package doc.
//
// A Ref is safe to log, and that property is load-bearing rather than
// incidental. It is what lets a repository location -- which carries two
// of these -- be part of an error message, a diagnostic dump or a
// structured log field without a custody review each time.
type Ref struct {
	// File is a path to a file holding the secret. Preferred, because a
	// file has an owner and a mode and the operating system enforces
	// both, and because it is the only source that does not put the
	// material into an environment block or a process argument list on
	// its way here.
	File string

	// Env names an environment variable holding the secret.
	//
	// The value is copied out and the copy is zeroed after use, but the
	// environment block itself cannot be scrubbed: Go keeps its own copy
	// and offers no way to erase it. An env source is therefore readable
	// by anything that can read this process's environment for as long as
	// the process lives, which is a real difference from File and the
	// reason File is preferred.
	Env string

	// Command is an argv array whose stdout is the secret. Command[0] is
	// the executable, invoked directly and never through a shell, so a
	// shell metacharacter anywhere in it is an inert literal byte rather
	// than a second command.
	//
	// This is how a secrets manager (OpenBao, Vault, SOPS, 1Password, AWS
	// Secrets Manager) is adopted without this project taking a
	// dependency on any of their SDKs or picking a winner among them.
	Command []string
}

// IsZero reports whether no source is declared.
func (r Ref) IsZero() bool {
	return r.File == "" && r.Env == "" && len(r.Command) == 0
}

// Validate reports whether exactly one source is declared.
func (r Ref) Validate() error {
	var n int

	if r.File != "" {
		n++
	}

	if r.Env != "" {
		n++
	}

	if len(r.Command) > 0 {
		n++
	}

	switch {
	case n == 0:
		return ErrNoSource
	case n > 1:
		return fmt.Errorf("%w: %s", ErrAmbiguousSource, r.declared())
	default:
		return nil
	}
}

// String names the source and never the material, which is what makes a
// Ref safe to put in a log line or an error.
//
// A command renders as its executable only. The arguments are omitted
// because a secrets-manager invocation's arguments routinely carry the
// path, the role or the token name, and an argv rendered in full is the
// most likely place for something credential-shaped to arrive in a log by
// accident.
func (r Ref) String() string {
	switch {
	case r.File != "":
		return "file " + r.File
	case r.Env != "":
		return "env " + r.Env
	case len(r.Command) > 0:
		return "command " + r.Command[0]
	default:
		return "no source"
	}
}

// declared lists every source that is set, for the ambiguity refusal. It
// exists so that refusal can say which two an operator wrote rather than
// only that there were two.
func (r Ref) declared() string {
	var parts []string

	if r.File != "" {
		parts = append(parts, "file "+r.File)
	}

	if r.Env != "" {
		parts = append(parts, "env "+r.Env)
	}

	if len(r.Command) > 0 {
		parts = append(parts, "command "+r.Command[0])
	}

	return strings.Join(parts, " and ")
}

// Resolve returns the material behind one reference as an opaque secret.
//
// Trailing whitespace is removed: every editor and every `echo >` leaves a
// newline behind, and a passphrase with an invisible newline on the end is
// a repository nobody can open, reported as a wrong password. Leading
// whitespace is removed for the same reason and with the same risk
// accepted -- a secret that deliberately begins or ends with a space
// cannot be expressed through a file, which is a limitation worth having
// over the alternative failure.
//
// The caller owns the lifetime of what comes back. Hold it for one
// operation, do not put it in a struct that outlives the operation, and do
// not resolve it earlier than the moment it is needed.
func Resolve(ctx context.Context, ref Ref) (obs.Secret, error) {
	raw, err := resolve(ctx, ref)
	if raw != nil {
		defer zero(raw)
	}

	if err != nil {
		return obs.Secret{}, err
	}

	text := strings.TrimSpace(string(raw))
	if text == "" {
		return obs.Secret{}, fmt.Errorf("%w: %s", ErrEmpty, ref)
	}

	return obs.NewSecret(text), nil
}

// resolve reads the bytes behind a reference. The returned slice is owned
// by the caller and is always zeroed by it, including on the error paths,
// because a command that failed after printing may well have printed the
// secret first.
func resolve(ctx context.Context, ref Ref) ([]byte, error) {
	if err := ref.Validate(); err != nil {
		return nil, err
	}

	switch {
	case ref.File != "":
		return fromFile(ref.File)
	case ref.Env != "":
		return fromEnv(ref.Env)
	default:
		return fromCommand(ctx, ref.Command)
	}
}

// fromFile reads a secret file under the same bound every other source
// obeys, applied to the read rather than to the result: io.ReadAll on an
// operator-supplied path would allocate whatever the file's size happens
// to be before anybody got the chance to object to it.
func fromFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		// err carries the path and the errno and nothing from inside the
		// file, which is the only thing that matters here.
		return nil, fmt.Errorf("secretref: opening secret file: %w", err)
	}

	defer f.Close() //nolint:errcheck // read-only

	// One byte past the limit, so a file exactly at the limit is accepted
	// and one over it is refused rather than silently truncated.
	buf, err := io.ReadAll(io.LimitReader(f, maxSecretSize+1))
	if err != nil {
		zero(buf)

		return nil, fmt.Errorf("secretref: reading secret file %s: %w", path, err)
	}

	if len(buf) > maxSecretSize {
		zero(buf)

		return nil, fmt.Errorf("%w: file %s exceeds %d bytes", ErrTooLarge, path, maxSecretSize)
	}

	return buf, nil
}

// fromEnv copies a secret out of the environment.
func fromEnv(name string) ([]byte, error) {
	val, ok := os.LookupEnv(name)
	if !ok {
		return nil, fmt.Errorf("secretref: environment variable %s is not set", name)
	}

	if len(val) > maxSecretSize {
		return nil, fmt.Errorf("%w: environment variable %s exceeds %d bytes", ErrTooLarge, name, maxSecretSize)
	}

	// []byte(val) copies. val is backed by Go's own copy of the process
	// environment block, which there is no supported way to zero, so the
	// copy is what this package can take responsibility for.
	return []byte(val), nil
}

// fromCommand runs a resolver command under the custody discipline every
// clause below is a hardening decision about:
//
//   - argv[0] is the executable and the rest are literal arguments;
//     exec.CommandContext never invokes a shell, so a metacharacter in any
//     element is inert.
//   - a fixed minimal environment rather than this process's own, because
//     a resolver is meant to be self-sufficient rather than handed ambient
//     state this manager holds for something unrelated.
//   - its own process group, so the timeout kills whatever it spawned.
//   - stdout bounded and never surfaced in an error; stderr bounded
//     separately and surfaced, because that is the diagnostic.
//
// The caller's ctx is honoured as well as the timeout: opening a
// repository is cancellable, and a resolver that outlived the cancelled
// operation would keep a backup window occupied after everybody stopped
// waiting for it.
func fromCommand(ctx context.Context, argv []string) ([]byte, error) {
	if strings.TrimSpace(argv[0]) == "" {
		return nil, errors.New("secretref: the declared command has no executable")
	}

	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	c := exec.CommandContext(ctx, argv[0], argv[1:]...)
	c.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin"}
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
	c.WaitDelay = reapBackstop

	stdout := &bounded{limit: maxSecretSize}
	stderr := &bounded{limit: maxCapturedStderr}
	c.Stdout = stdout
	c.Stderr = stderr

	runErr := c.Run()

	// Whatever happened, the material the command may already have printed
	// is in stdout, and it is the caller's to zero. Every error path below
	// returns it for exactly that reason.
	out := stdout.buf.Bytes()

	switch {
	case ctx.Err() != nil:
		return out, fmt.Errorf("secretref: command %s: killed after exceeding its %s timeout", argv[0], commandTimeout)
	case runErr != nil:
		return out, fmt.Errorf("secretref: command %s: %v (stderr: %s)", argv[0], runErr, stderr.buf.String())
	case stdout.truncated:
		return out, fmt.Errorf("%w: command %s exceeded %d bytes", ErrTooLarge, argv[0], maxSecretSize)
	}

	return out, nil
}

// bounded caps how many bytes of a subprocess stream it retains.
//
// Write reports len(p) whatever it kept, which is the whole point: an
// io.Writer that returned a short count would make os/exec treat the
// truncation as a write error and tear down the pipe, so a resolver that
// printed too much would look like one that crashed. Everything past the
// limit is dropped, truncated records that it happened, and the caller
// refuses rather than using a prefix.
type bounded struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (w *bounded) Write(p []byte) (int, error) {
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

// zero overwrites b in place. The KeepAlive is there so the compiler
// cannot prove the writes are dead and elide them, which is as far as
// "zeroed where Go allows" reaches: there is no memory-locking or
// guaranteed-erase primitive, so this defends against an accidental later
// reuse of freed memory -- a heap dump, a debugger on a stale allocation
// -- and not against an attacker already inside this process.
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}

	runtime.KeepAlive(b)
}
