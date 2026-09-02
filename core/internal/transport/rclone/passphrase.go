// This file is #269's passphrase resolvers: the same three sources (file,
// env, command) config.Key.Passphrase offers for the key material itself,
// reused here for the secret that unlocks it. ssh.go's sftpConfig calls
// resolvePassphrase once, before it resolves the key material, and only
// acts on the result when Source actually names one of the three; a
// Source with none of them set is read as "this key is not passphrase-
// protected", exactly the case every Source predates #269 in.
//
// # Why this cannot keep key_file's "never enters this process's memory" property
//
// Key.File's whole security property is that rclone opens the file itself:
// this process never reads it. There is no equivalent for a passphrase.
// rclone's sftp backend has no "read the passphrase from this file"
// option; the only thing it accepts is key_file_pass, the passphrase text
// itself (obscured; see ssh.go). So all three sources below are read by
// THIS process, never handed to rclone as a path, and this file does not
// pretend that keeps a passphrase out of memory the way key_file keeps the
// key material out.
//
// # Why a resolved passphrase gets a trailing newline trimmed
//
// A single trailing "\r\n" or "\n" is trimmed from a file- or command-
// resolved passphrase, never from an environment variable (nothing in the
// shell convention that sets one ever appends a newline to its value the
// way `echo` does to a line of output). `echo "secret" > passfile` and a
// `printf`/`echo`-based command resolver are both far more natural to
// write than something that carefully avoids ever emitting one, and a
// literal trailing newline silently becoming part of the passphrase, so
// that a passphrase that is otherwise correct fails to decrypt, is a
// footgun with no compensating security benefit.
package rclone

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/rclone/rclone/lib/env"

	"github.com/spdrman/rclone-manager/core/internal/obs"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// passphraseCommandTimeout mirrors keyCommandTimeout: a passphrase
// resolver command is the same shape of work as a key resolver command
// (one secrets-manager round trip: read one secret, print it, exit), so it
// gets the same bound. It is a var, not a const, for the same reason
// keyCommandTimeout is: so a test can shorten it without an actual
// multi-second sleep.
var passphraseCommandTimeout = 15 * time.Second

// maxResolvedPassphraseSize bounds how many bytes of a command's stdout,
// or a passphrase file's content, this resolver will accept. Far more
// generous than any realistic passphrase needs, on the same reasoning
// keysource.go's maxResolvedKeySize states for key material: this is a
// margin against a misbehaving or compromised resolver, not a realistic
// ceiling.
const maxResolvedPassphraseSize = 1 << 16 // 64 KiB

// resolvePassphrase reads src's configured passphrase source, if any, and
// returns it wrapped in obs.Secret. ok is false, with a nil err, when none
// of PassphraseFile/PassphraseEnv/PassphraseCommand is set: the normal
// case for an unencrypted key, and never itself an error. Exactly one of
// the three being set is enforced by internal/config's own Validate for a
// config built through that package; this is the same backstop
// sftpConfig's own key-source count check already is for a
// transport.Source built directly, tests included.
func resolvePassphrase(src transport.Source) (secret obs.Secret, ok bool, err error) {
	switch {
	case src.PassphraseFile != "":
		secret, err = resolvePassphraseFromFile(src.PassphraseFile)
	case src.PassphraseEnv != "":
		secret, err = resolvePassphraseFromEnv(src.PassphraseEnv)
	case len(src.PassphraseCommand) > 0:
		secret, err = resolvePassphraseFromCommand(src.PassphraseCommand)
	default:
		return obs.Secret{}, false, nil
	}
	if err != nil {
		return obs.Secret{}, false, err
	}
	return secret, true, nil
}

// resolvePassphraseFromFile reads path (shell-expanded exactly like
// Key.File and KnownHosts already are) and returns its content, trailing
// newline trimmed, wrapped in obs.Secret.
func resolvePassphraseFromFile(path string) (obs.Secret, error) {
	expanded := env.ShellExpand(path)
	raw, err := os.ReadFile(expanded)
	if err != nil {
		return obs.Secret{}, fmt.Errorf("passphrase file %q: %w", path, err)
	}
	defer zeroBytes(raw)

	trimmed := strings.TrimRight(string(raw), "\r\n")
	if trimmed == "" {
		return obs.Secret{}, fmt.Errorf("passphrase file %q: resolved passphrase is empty", path)
	}
	return obs.NewSecret(trimmed), nil
}

// resolvePassphraseFromEnv reads name from the environment and returns it
// wrapped in obs.Secret, unmodified: see this file's package doc for why
// an environment variable's value is not trimmed the way a file's or a
// command's output is.
func resolvePassphraseFromEnv(name string) (obs.Secret, error) {
	val, ok := os.LookupEnv(name)
	if !ok {
		return obs.Secret{}, fmt.Errorf("environment variable %q is not set", name)
	}
	if val == "" {
		return obs.Secret{}, fmt.Errorf("environment variable %q: resolved passphrase is empty", name)
	}
	return obs.NewSecret(val), nil
}

// resolvePassphraseFromCommand runs argv and treats its stdout, trailing
// newline trimmed, as the passphrase. It mirrors resolveKeyFromCommand's
// subprocess posture exactly (no shell, a fixed minimal environment, its
// own process group so a bounded timeout can kill anything it spawned,
// bounded captured output) rather than sharing code with it: this
// project's own convention, stated in internal/app's sha256File doc, is
// that a small (here, well under fifty lines) resolver-shaped duplicate is
// preferred over a cross-cutting shared helper for two call sites that
// happen to look similar today but answer different questions (a key
// resolver's caller cares about the exact bytes it gets back; this one
// trims and treats them as text).
func resolvePassphraseFromCommand(argv []string) (obs.Secret, error) {
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return obs.Secret{}, errors.New("passphrase command: no executable configured")
	}

	ctx, cancel := context.WithTimeout(context.Background(), passphraseCommandTimeout)
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
	c.WaitDelay = 5 * time.Second

	stdout := &boundedBuffer{limit: maxResolvedPassphraseSize}
	stderr := &boundedBuffer{limit: maxCapturedStderr}
	c.Stdout = stdout
	c.Stderr = stderr
	defer stdout.zero()

	runErr := c.Run()

	switch {
	case ctx.Err() != nil:
		return obs.Secret{}, fmt.Errorf("passphrase command %q: killed after exceeding its %s timeout", argv[0], passphraseCommandTimeout)
	case runErr != nil:
		return obs.Secret{}, fmt.Errorf("passphrase command %q: %v (stderr: %s)", argv[0], runErr, stderr.String())
	case stdout.truncated:
		return obs.Secret{}, fmt.Errorf("passphrase command %q: output exceeded %d bytes, refusing to treat truncated output as a passphrase", argv[0], maxResolvedPassphraseSize)
	}

	trimmed := strings.TrimRight(stdout.buf.String(), "\r\n")
	if trimmed == "" {
		return obs.Secret{}, fmt.Errorf("passphrase command %q: resolved passphrase is empty", argv[0])
	}
	return obs.NewSecret(trimmed), nil
}
