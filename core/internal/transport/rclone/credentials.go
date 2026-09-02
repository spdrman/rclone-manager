// Package rclone: this file is FR-33's runtime half, the storage-medium
// counterpart to keysource.go.
//
// config.MediumCredentials already decided the schema half: three sources
// naming where a secret comes FROM, and no field at all it could be pasted
// INTO. This file is what happens when one of those three is actually
// resolved, and it deliberately reuses keysource.go's machinery rather than
// standing up a second one beside it. The command runner is literally the
// same function (runResolverCommand, factored out of resolveKeyFromCommand
// for this), the memory hygiene is the same, and the rule about what a
// failure may say is the same.
//
// # The three sources are not equal, and file is preferred for a real reason
//
// An SSH key's key_file property is that rclone opens the file, so the key
// never enters this process's memory. The same property holds here and it
// matters more: an S3 credential unlocks every retained artifact on the
// medium at once, where an SSH key unlocks one hardened source account. So
// `file` hands rclone a PATH (its own shared_credentials_file option) and
// this process never reads the bytes, while `env` and `command` necessarily
// route the material through here, exactly as key_pem does for an SSH key.
//
// # What "validated by shape" means here
//
// FR-33 says whatever env or command produce is validated by shape before
// use. The shape is the AWS shared-credentials format, which is the same
// format the file source's file is in: one credential format, three ways to
// deliver it, rather than three formats to keep in agreement. A secrets
// manager answering with an HTML login page, a JSON blob, an error string
// or an empty body fails HERE, at the point the bytes were produced, with a
// message naming the kind of problem rather than the bytes that had it.
//
// # What a failure may say
//
// Never the resolved bytes, on any path, for any reason. Not the value that
// failed to parse, not a prefix of it, not its length. A KEY NAME is
// different (an unknown `region =` line is named, because a key name is not
// a secret and the operator has to know which line to remove), and a
// command's stderr is different (diagnostic text by convention, and the
// only thing an operator debugging a broken resolver has). credentials_test.go
// plants a canary in every source and hunts for it in every error.
package rclone

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/rclone/rclone/lib/env"

	"github.com/spdrman/rclone-manager/core/internal/obs"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// maxResolvedCredentialsSize bounds an env value or a command's stdout on
// the credentials path. A shared-credentials block is three short lines;
// 64 KiB is a wide margin meant to stop a misbehaving resolver exhausting
// memory, not a realistic ceiling. It is far tighter than
// maxResolvedKeySize because a credential block has no legitimate reason to
// approach an RSA-4096 PEM's size, and a bound that fits the actual shape
// of the data is a better bound.
const maxResolvedCredentialsSize = 64 << 10

// The three keys an AWS shared-credentials profile may carry, as far as
// this product is concerned. Spelled in lower case; the parser folds case
// before comparing, because a real credentials file written by hand or by
// another tool uses either.
const (
	credKeyAccessKeyID     = "aws_access_key_id"
	credKeySecretAccessKey = "aws_secret_access_key"
	credKeySessionToken    = "aws_session_token"
)

// configErrorf builds a transport.Configuration failure.
//
// Every refusal in this file and in s3.go that is a fact about what an
// operator WROTE goes through it, rather than falling through Classify to
// Permanent. The two are different messages to whoever is on the other end:
// Permanent means this code did not recognise the failure, and
// Configuration means somebody has a typo and no retry will help. FR-28
// added the category for exactly this, and a category nothing produces is
// a category nothing can act on.
//
// A permission failure is NOT one of these: it keeps transport.KeyPermissions,
// which is its own considered verdict with its own remediation.
func configErrorf(format string, args ...any) error {
	return transport.NewError(transport.Configuration, "medium_configuration", fmt.Errorf(format, args...))
}

// resolvedCredentials is one medium's credentials, after resolution.
//
// # Why this type carries its own redaction, when its fields are already obs.Secret
//
// Because obs.Secret is not enough here, and finding that out is the
// clearest thing the canary test in this package has done. obs.Secret
// forecloses every fmt verb by implementing fmt.Formatter, and fmt honours
// that for a Secret it is handed directly. It does NOT honour it for a
// Secret sitting in an UNEXPORTED field of some other struct: fmt reflects
// into the struct, cannot take an interface from an unexported field
// (reflect.Value.CanInterface is false), so it never asks about Formatter
// and prints the underlying data instead. Measured, before this type had
// any methods of its own:
//
//	fmt.Sprintf("%v", resolvedCredentials{...})  =>  {{CANARY-SECRET-...} ...}
//	fmt.Sprintf("%+v", resolvedCredentials{...}) =>  {secretAccessKey:{v:CANARY-SECRET-...} ...}
//
// which is precisely the 2am reflex obs.Secret exists to defeat, defeating
// obs.Secret instead. An EXPORTED field of the same type renders
// [REDACTED] correctly, so this is a property of field visibility and not
// of Secret.
//
// So the redaction is reasserted at THIS level, where the fields are
// reachable, using the same five interfaces obs.Secret itself implements
// and for the same reasons its doc gives. obs.Secret's own doc has been
// corrected to state the limitation, and internal/obs has a test that pins
// it, so the next person to wrap a secret in an unexported field reads it
// before they need it rather than after.
//
// sharedFile is a path, not a secret, and is deliberately NOT redacted: a
// misconfigured medium has to be debuggable, and the path is exactly what
// an operator needs to see.
type resolvedCredentials struct {
	// usingSharedFile selects which half of this struct is meaningful. It
	// is a field rather than "sharedFile != \"\"" so a caller reads the
	// decision instead of re-deriving it, which is the same reason
	// sftpConfig carries usingKeyPEM beside keyPEM.
	usingSharedFile bool

	// sharedFile is the expanded path handed to rclone's
	// shared_credentials_file option. Set only for the File source, and
	// the only field set in that case: this process holds the path and
	// never the contents.
	sharedFile string

	// accessKeyID, secretAccessKey and sessionToken are set only for the
	// Env and Command sources. The access key id is wrapped alongside the
	// secret even though it is closer to a username than a password,
	// because FR-33's list says a credential may not appear in a log line
	// "in whole or in part" and half a credential in a log is still half
	// a credential in a log.
	accessKeyID     obs.Secret
	secretAccessKey obs.Secret
	sessionToken    obs.Secret

	// hasSessionToken distinguishes "no token was supplied" from "a token
	// was supplied and it is the empty string", which matters because
	// s3.go only sets rclone's session_token option when there genuinely
	// is one: setting it empty is not the same as not setting it.
	hasSessionToken bool
}

// redactedCredentials is what a resolvedCredentials renders as, whichever
// interface ends up being asked. It names the source so a log line is still
// worth having: which of the three sources was in play, and (for the file
// source only) which path, are both facts an operator debugging a medium
// needs and neither is a secret.
func (c resolvedCredentials) redacted() string {
	if c.usingSharedFile {
		return fmt.Sprintf("medium credentials{source: shared file %q, read by rclone, never by this process}", c.sharedFile)
	}
	return "medium credentials{source: resolved into memory, [REDACTED]}"
}

// String implements fmt.Stringer.
func (c resolvedCredentials) String() string { return c.redacted() }

// GoString implements fmt.GoStringer, for %#v.
func (c resolvedCredentials) GoString() string { return c.redacted() }

// Format implements fmt.Formatter, which is the one that actually matters:
// fmt consults it before any other interface and before applying
// verb-specific behaviour, so it closes off every verb at once rather than
// the common ones. See obs.Secret's own Format for the long version of the
// argument.
func (c resolvedCredentials) Format(f fmt.State, _ rune) {
	_, _ = f.Write([]byte(c.redacted()))
}

// MarshalJSON implements json.Marshaler, so encoding/json (and the path
// log/slog's JSON handler falls back to) never serialises the fields.
func (c resolvedCredentials) MarshalJSON() ([]byte, error) {
	return json.Marshal(c.redacted())
}

// LogValue implements slog.LogValuer, which log/slog resolves before it
// does anything else with a value.
func (c resolvedCredentials) LogValue() slog.Value {
	return slog.StringValue(c.redacted())
}

// resolveMediumCredentials turns a medium's credential reference into
// something s3Config can use, or refuses.
//
// It enforces "exactly one source" as a backstop, exactly as sftpConfig
// does for an sftp Source's three key sources and for the same reason:
// internal/config/validate.go enforces the identical rule independently, so
// a config built through that package never arrives here with more than
// one set, and this catches everything that builds a transport.Medium
// directly, tests included. Two configured sources is a mistake, not a
// precedence order for this adapter to silently pick through.
func resolveMediumCredentials(medium transport.Medium) (resolvedCredentials, error) {
	sources := 0
	if medium.Credentials.File != "" {
		sources++
	}
	if medium.Credentials.Env != "" {
		sources++
	}
	if len(medium.Credentials.Command) > 0 {
		sources++
	}
	switch {
	case sources == 0:
		return resolvedCredentials{}, configErrorf("medium %q: exactly one of credentials.file, credentials.env or credentials.command is required (there is no anonymous access and no ambient-credential fallback)", medium.ID)
	case sources > 1:
		return resolvedCredentials{}, configErrorf("medium %q: exactly one of credentials.file, credentials.env or credentials.command may be set, not more than one", medium.ID)
	}

	switch {
	case medium.Credentials.File != "":
		path, err := resolveCredentialsFile(medium.ID, medium.Credentials.File)
		if err != nil {
			return resolvedCredentials{}, err
		}
		return resolvedCredentials{usingSharedFile: true, sharedFile: path}, nil

	case medium.Credentials.Env != "":
		creds, err := resolveCredentialsFromEnv(medium.Credentials.Env)
		if err != nil {
			return resolvedCredentials{}, fmt.Errorf("medium %q: resolving credentials from environment variable %q: %w", medium.ID, medium.Credentials.Env, err)
		}
		return creds, nil

	default:
		creds, err := resolveCredentialsFromCommand(medium.Credentials.Command)
		if err != nil {
			return resolvedCredentials{}, fmt.Errorf("medium %q: resolving credentials from the configured command: %w", medium.ID, err)
		}
		return creds, nil
	}
}

// resolveCredentialsFile checks the file this adapter will hand to rclone
// and returns its expanded path. It never opens it.
//
// # Why the mode check is "no wider than", where the SSH key's is exact
//
// checkKeyFileMode refuses anything that is not exactly 0600, and its own
// doc explains why: importSSHKeyInto wrote that file with 0600, so any
// other mode is drift away from something this program did, and "verify
// what actually happened" is the whole point.
//
// Nothing writes a credentials file yet. #235 is the runtime half of
// FR-33; the API import flow that will put one into private state is still
// ahead, and until it exists the only way this file gets on disk is an
// operator creating it. So there is no written-with value to compare
// against, and an exact check would refuse a perfectly sensible 0400 for no
// reason anyone could act on. What the check can still say with total
// confidence is the thing that actually matters: nobody but the owner may
// read or write it. When the import flow lands and this file has a mode
// this program chose, tightening this to an exact match is the right change
// and it belongs in that issue, where the value being compared against
// exists.
//
// The directory-chain check is shared outright with the SSH key path
// (checkKeyDirChainMode), because the argument there transfers with nothing
// to adjust: a file at a pristine 0600 inside a world-writable directory
// can be unlinked and replaced by any local actor regardless of its own
// mode, and that is a swap this process could not detect.
func resolveCredentialsFile(mediumID, configured string) (string, error) {
	path := env.ShellExpand(configured)
	info, err := os.Stat(path)
	if err != nil {
		return "", configErrorf("medium %q: credentials file %q is not accessible: %v", mediumID, configured, err)
	}
	if info.IsDir() {
		return "", configErrorf("medium %q: credentials file %q is a directory, not a file", mediumID, configured)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return "", transport.NewError(transport.KeyPermissions, "medium_credentials_permissions", fmt.Errorf(
			"medium %q: credentials file %q has permissions %04o, which lets somebody other than its owner read or write it: "+
				"these credentials unlock every artifact retained on this medium, so correct it (chmod go-rwx %s) before using it",
			mediumID, configured, mode, configured,
		))
	}
	if err := checkKeyDirChainMode(mediumID, configured, path); err != nil {
		return "", err
	}
	if err := checkCredentialsFileHasADefaultProfile(mediumID, configured, path); err != nil {
		return "", err
	}
	return path, nil
}

// checkCredentialsFileHasADefaultProfile refuses a credentials file that
// does not carry a profile named exactly `default`.
//
// # Why this exists, and it is not a style rule
//
// Measured against a real MinIO. A file whose only profile is
// `[cold-storage]`, or `[profile cold-storage]`, does not fail: it HANGS.
// rclone reaches the file through the AWS SDK's credential chain, the
// chain finds no profile it was asked for, and it keeps walking, and the
// last link is EC2 instance metadata. On any host that is not an EC2
// instance, that is a connection to the link-local address 169.254.169.254
// which nothing answers, so the operation stalls for as long as the caller
// allows it to. Timed: 12 seconds against a 12-second deadline, and it
// would have been an hour against an hour.
//
//	default.creds     took=7ms       err=<nil>
//	named.creds       took=12.002s   err=... context deadline exceeded
//	profiled.creds    took=12s       err=... context deadline exceeded
//
// A backup medium that stalls silently is precisely the
// protection-dies-quietly failure this product exists to prevent, and a
// deployment would experience it as "backups got slow" rather than as a
// typo in a profile name. There is no rclone option and no SDK option
// reachable from here that shortens it: the SDK's own switch is the
// AWS_EC2_METADATA_DISABLED environment variable, and a library may not
// mutate its process's environment.
//
// # What this reads, and what it does not
//
// This is the one place this process opens a credentials file, so the
// property that makes `file` the preferred source deserves restating
// exactly rather than being quietly weakened.
//
// It reads the file's LINES to find its profile headers. It retains only
// the header names, compares them against one literal, and zeroes its
// buffer on the way out. It never parses a setting, never wraps a value,
// never returns one, and never hands one to rclone: rclone still opens the
// file itself for the actual credentials, which is the part that matters.
// A key's bytes do pass through a buffer this function owns for the
// duration of one scan and are then overwritten, which is a strictly
// smaller exposure than the env and command sources accept for their whole
// operation.
//
// The trade is worth naming plainly: a few microseconds of a secret in a
// buffer this code zeroes, against a medium that hangs instead of failing.
//
// # Why `default` and not "any single profile"
//
// Because there is no profile field in config.MediumCredentials to select
// with, and rclone passes the configured path through
// awsconfig.WithSharedConfigFiles, so the SDK looks for whichever profile
// it was told to use, which with nothing set is `default`. The env and
// command sources accept a single profile under any name because THIS
// package parses those itself and can simply take the one profile there
// is; the file source's selection happens inside the SDK, where this
// package has no say.
func checkCredentialsFileHasADefaultProfile(mediumID, configured, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return configErrorf("medium %q: credentials file %q could not be read: %v", mediumID, configured, err)
	}
	defer f.Close()

	var profiles []string
	found := false
	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 4096)
	scanner.Buffer(buf, maxResolvedCredentialsSize)
	defer zeroBytes(buf[:cap(buf)])
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "[") || !strings.HasSuffix(line, "]") {
			// Not a header. Nothing about this line is looked at, kept,
			// or reported.
			continue
		}
		name := strings.TrimSpace(line[1 : len(line)-1])
		profiles = append(profiles, name)
		if name == "default" {
			found = true
		}
	}
	if err := scanner.Err(); err != nil {
		return configErrorf("medium %q: credentials file %q could not be read as text", mediumID, configured)
	}
	if found {
		return nil
	}
	// The profile NAMES are reported, because a name is not a secret and is
	// the whole content of the fix. No value ever is.
	switch len(profiles) {
	case 0:
		return configErrorf("medium %q: credentials file %q has no [profile] header at all; it must carry a [default] profile, "+
			"because there is no profile setting to select any other one with", mediumID, configured)
	default:
		return configErrorf("medium %q: credentials file %q declares %v but no [default] profile. "+
			"Rename it to [default]: there is no profile setting to select another one with, and a file the credential chain "+
			"cannot resolve does not fail, it falls through to EC2 instance metadata and stalls until the operation times out",
			mediumID, configured, profiles)
	}
}

// resolveCredentialsFromEnv reads name from the environment and validates
// it by shape.
//
// The []byte copy and the zeroing that follows are keysource.go's
// resolveKeyFromEnv's, for its reason: the string this reads is backed by
// Go's own copy of the process environment block, shared with every other
// lookup of the same variable, and there is no supported way to zero that
// without corrupting it for whoever reads it next. buf is a copy this
// function owns outright, so it is what gets overwritten.
func resolveCredentialsFromEnv(name string) (resolvedCredentials, error) {
	val, ok := os.LookupEnv(name)
	if !ok {
		return resolvedCredentials{}, configErrorf("environment variable %q is not set", name)
	}
	buf := []byte(val)
	defer zeroBytes(buf)
	return validateAndWrapCredentials(buf)
}

// resolveCredentialsFromCommand runs argv and treats its stdout as
// shared-credentials text.
//
// argv[0] is the executable; the rest are its literal arguments. There is
// no shell anywhere in this call, so a metacharacter in any element is
// inert. See runResolverCommand for the full discipline, which this shares
// byte for byte with the SSH key resolver.
func resolveCredentialsFromCommand(argv []string) (resolvedCredentials, error) {
	stdout, done, err := runResolverCommand(argv, "credentials command", "credential material", maxResolvedCredentialsSize)
	if err != nil {
		return resolvedCredentials{}, err
	}
	defer done()

	creds, verr := validateAndWrapCredentials(stdout)
	if verr != nil {
		return resolvedCredentials{}, fmt.Errorf("credentials command %q: %w", argv[0], verr)
	}
	return creds, nil
}

// validateAndWrapCredentials is the one place raw resolver output is
// checked and, if it passes, wrapped. Both resolvers above call this and
// only this, so there is one answer to "are these usable credentials"
// rather than two that can disagree.
//
// raw is never included in a returned error. Whether it is empty, an HTML
// login page, a JSON blob a differently-shaped secrets manager produced, or
// a genuinely correct credentials block with one field missing, the failure
// is reported by naming the kind of problem, never by echoing the bytes
// that had it. This is validateAndWrapKey's rule, transposed.
func validateAndWrapCredentials(raw []byte) (resolvedCredentials, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return resolvedCredentials{}, fmt.Errorf("resolved credential material is empty")
	}

	fields, err := parseSharedCredentials(raw)
	if err != nil {
		return resolvedCredentials{}, err
	}

	if fields[credKeyAccessKeyID] == "" {
		return resolvedCredentials{}, fmt.Errorf("resolved credential material has no %s", credKeyAccessKeyID)
	}
	if fields[credKeySecretAccessKey] == "" {
		return resolvedCredentials{}, fmt.Errorf("resolved credential material has no %s", credKeySecretAccessKey)
	}

	creds := resolvedCredentials{
		accessKeyID:     obs.NewSecret(fields[credKeyAccessKeyID]),
		secretAccessKey: obs.NewSecret(fields[credKeySecretAccessKey]),
	}
	if token, ok := fields[credKeySessionToken]; ok && token != "" {
		creds.sessionToken = obs.NewSecret(token)
		creds.hasSessionToken = true
	}
	return creds, nil
}

// parseSharedCredentials reads the AWS shared-credentials format: a profile
// header in square brackets, then key = value lines.
//
// # Why this repository parses an INI format by hand
//
// Because the alternative is a dependency whose job is to be permissive,
// and permissive is the opposite of what is wanted here. Every INI library
// worth using accepts things this must refuse: several profiles with a
// documented precedence rule, keys this product has no meaning for,
// value-continuation lines. Each of those is a way for a resolver's output
// to be interpreted differently from how its author read it, and this is
// the one place where being interpreted differently means using the wrong
// credentials. Forty lines that refuse everything not explicitly allowed is
// the smaller risk.
//
// # Exactly one profile, and it may have any name
//
// A real ~/.aws/credentials holds several profiles and the caller picks by
// name. There is no profile field in config.MediumCredentials to pick with,
// so a block with two profiles has no defensible answer: choosing
// [default], or the first, would be this adapter guessing at intent, and
// guessing about which credentials to use is guessing about which account
// gets billed and which bucket gets written. One profile is unambiguous
// whatever it is called, so that is what is required.
//
// # Unknown keys are refused, by name
//
// This is the KnownFields(true) rule config.Load applies to the same
// secrets, brought to a format that has no such mechanism of its own. A
// `region = us-west-2` line that this adapter silently ignored would be an
// operator's explicit instruction quietly not happening, and a region
// belongs on the medium's own config where it is visible. The key's NAME is
// safe to report and is exactly what the operator needs; its value never
// is, and never appears.
func parseSharedCredentials(raw []byte) (map[string]string, error) {
	fields := map[string]string{}
	profiles := 0

	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 4096), maxResolvedCredentialsSize)
	line := 0
	for scanner.Scan() {
		line++
		// TrimSpace also removes the \r a file that travelled through
		// Windows carries, so CRLF costs nothing here.
		text := strings.TrimSpace(scanner.Text())
		switch {
		case text == "", strings.HasPrefix(text, "#"), strings.HasPrefix(text, ";"):
			continue
		case strings.HasPrefix(text, "[") && strings.HasSuffix(text, "]"):
			profiles++
			if profiles > 1 {
				return nil, fmt.Errorf("resolved credential material declares more than one profile, and there is no profile setting to choose between them; supply exactly one")
			}
			continue
		}

		if profiles == 0 {
			return nil, fmt.Errorf("resolved credential material has a setting on line %d before any [profile] header; the AWS shared-credentials format needs one", line)
		}

		key, value, found := strings.Cut(text, "=")
		if !found {
			// The LINE NUMBER, never the line. A line with no "=" is
			// very often a resolver's error text or the first line of an
			// HTML page, and printing it back is printing whatever the
			// resolver decided to say.
			return nil, fmt.Errorf("resolved credential material has a line %d that is not a `key = value` setting", line)
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)

		switch key {
		case credKeyAccessKeyID, credKeySecretAccessKey, credKeySessionToken:
			if _, dup := fields[key]; dup {
				return nil, fmt.Errorf("resolved credential material sets %s twice", key)
			}
			fields[key] = value
		default:
			return nil, fmt.Errorf("resolved credential material sets %q, which this product does not read; a medium's region, endpoint and storage class belong in its own configuration, and only %s, %s and %s are accepted here",
				key, credKeyAccessKeyID, credKeySecretAccessKey, credKeySessionToken)
		}
	}
	if err := scanner.Err(); err != nil {
		// Deliberately not %w and deliberately not the scanner's own text
		// verbatim: bufio.Scanner's oversized-line error is safe, but this
		// is the one place a future scanner error could carry content, and
		// the shape of the problem is all a caller needs.
		return nil, fmt.Errorf("resolved credential material could not be read as text")
	}
	if profiles == 0 {
		return nil, fmt.Errorf("resolved credential material has no [profile] header; the AWS shared-credentials format needs one")
	}
	return fields, nil
}
