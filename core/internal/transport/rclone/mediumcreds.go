// This file is FR-33's runtime half: turning a medium's three credential
// REFERENCES into something rclone's s3 backend can authenticate with,
// under exactly the custody discipline keysource.go already built for SSH
// private keys.
//
// The three sources and their trade-off are the same trade-off, and the
// preference order is the same order:
//
//   - `file` is preferred, and it is preferred harder here than it is for
//     an SSH key. rclone opens the file itself (the s3 backend's own
//     env_auth + shared_credentials_file options), so the CREDENTIAL never
//     enters this process's memory, and a secret this process never holds
//     is a secret it cannot log, cannot leak through a %+v, and cannot
//     leave behind in a heap dump.
//
//     That statement used to say "the file's bytes never enter this
//     process", which stopped being true when the profile check landed,
//     so it is narrowed rather than left standing.
//     checkCredentialsFileHasADefaultProfile opens the file to read its
//     profile HEADERS, because a file the AWS credential chain cannot
//     resolve does not fail, it stalls on instance metadata until the
//     operation times out. That function's doc states exactly what its
//     scan reads, what it retains, and what it does not claim about
//     erasing it. Nothing else in this package opens a credentials file.
//
//   - `env` and `command` cannot have that property, by construction, so
//     everything downstream of the moment the bytes exist is made as
//     narrow as keysource.go makes it: validated by SHAPE before anything
//     wraps them, wrapped in obs.Secret, never echoed in an error, and
//     zeroed where this file owns the memory outright.
//
// # One format, three sources
//
// All three produce the SAME thing: AWS shared-credentials text, the
// `[default]` / `aws_access_key_id` / `aws_secret_access_key` form. That
// is not an arbitrary choice, it is the only one that keeps `file` honest:
// `file` is handed to rclone unopened, so its format is decided by the AWS
// SDK rclone hands it to, and inventing a second, friendlier format for
// `env` and `command` would mean this product had two credential formats
// whose difference nobody could see until one of them failed in
// production.
//
// # What is never echoed
//
// A resolver failure is reported by the SHAPE of the problem (empty, not
// credentials text at all, no usable profile, a profile with no key),
// never by the content that failed. That is keysource.go's rule verbatim
// and it exists for the same reason: a secrets manager answering with an
// HTML login page, a truncated read, and a genuinely valid secret are
// indistinguishable to this code, so none of them is ever quoted back.
// A command's STDERR is diagnostic text by convention and is surfaced, on
// the same distinction keysource.go already draws.
package rclone

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/rclone/rclone/fs/config/configmap"

	"github.com/spdrman/rclone-manager/core/internal/obs"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// maxResolvedCredentialsSize bounds how many bytes of a command's stdout,
// or an environment variable's value, this resolver will accept as
// credentials text. A shared-credentials file with one profile is a few
// hundred bytes; this is a wide margin meant to stop a misbehaving or
// compromised resolver from exhausting memory, not a realistic ceiling.
const maxResolvedCredentialsSize = 64 << 10 // 64 KiB

// resolvedCredentials is a validated credential set on its way into
// rclone's own options, and nowhere else.
//
// All three fields are obs.Secret, the access key id included. FR-33 says
// a credential must not reach a log "in whole or in part", and an access
// key id is a part: it identifies the principal, it is what an attacker
// needs the other half of, and there is no diagnostic this product wants
// badly enough to print one for.
type resolvedCredentials struct {
	accessKeyID     obs.Secret
	secretAccessKey obs.Secret
	sessionToken    obs.Secret
	hasSessionToken bool
}

// The four renderings below exist because obs.Secret does NOT protect a
// value in an UNEXPORTED field, and every field above is unexported.
//
// fmt cannot take an interface out of an unexported struct field
// (reflect.Value.CanInterface is false for one), so it never asks whether
// the field implements Formatter and prints the wrapped string instead.
// Measured on this exact type, before these methods existed:
//
//	fmt.Sprintf("%+v", resolvedCredentials{...})
//	  => {accessKeyID:{v:AKIAPROBEKEY} secretAccessKey:{v:probesecretvalue} ...}
//
// That is FR-33's "never in a log line, in whole or in part" defeated by a
// reflex %+v in a debug statement nobody would look twice at in review, so
// redaction is reasserted here rather than left to the wrapper that cannot
// reach it. obs.Secret's own doc now records the limitation, and
// obs.TestSecretInAnUnexportedFieldStillLeaks pins it.
// String covers %s and %v on a value whose fields fmt would otherwise
// walk; GoString covers %#v, which consults GoStringer ahead of anything
// else and is the verb a debugger-minded print reaches for precisely
// because it shows structure.
func (c resolvedCredentials) String() string   { return "[REDACTED]" }
func (c resolvedCredentials) GoString() string { return "[REDACTED]" }

// Format takes precedence over String for every fmt verb, including %+v
// and %#v, which are the two that actually reach for a struct's fields.
func (c resolvedCredentials) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, "[REDACTED]")
}

// MarshalJSON covers encoding/json, including the path slog's JSON handler
// takes for a value it does not otherwise recognise.
func (c resolvedCredentials) MarshalJSON() ([]byte, error) { return []byte(`"[REDACTED]"`), nil }

// LogValue is log/slog's own hook, consulted before it asks a Formatter or
// a Stringer anything.
func (c resolvedCredentials) LogValue() slog.Value { return slog.StringValue("[REDACTED]") }

// credentialsUnavailable is the one constructor for "this adapter could
// not obtain the credential this medium declares", so every path that
// reaches that conclusion carries the same category, the same op AND the
// same sentinel (transport.ErrCredentialsUnavailable).
//
// The sentinel is what lets a caller tell this apart from the other
// Configuration failure a medium produces, a bucket that is not there,
// without reading Error.Op or matching on text. See the sentinel's own doc
// for why it is not a new category.
//
// It wraps rather than replaces: cause keeps saying exactly what went
// wrong, for the log, and this package's own rule about what a cause may
// say is unchanged (see this file's "What is never echoed").
func credentialsUnavailable(cause error) error {
	return transport.NewError(transport.Configuration, "medium_credentials",
		fmt.Errorf("%w: %w", transport.ErrCredentialsUnavailable, cause))
}

// mediumAuthOptions returns the rclone s3 options that authenticate medium,
// resolving whichever of the three credential sources it names.
//
// Exactly one source must be set. "Exactly one", not "at least one", for
// sftpConfig's reason: two configured sources is a config mistake, not a
// precedence order for this adapter to silently pick through.
// core/internal/config enforces the same rule independently, so a config
// built through that package never arrives here with more than one; this
// is the backstop for anything that builds a transport.Medium directly,
// tests included.
func mediumAuthOptions(medium transport.Medium) (configmap.Simple, error) {
	creds := medium.Credentials

	sourceCount := 0
	if creds.File != "" {
		sourceCount++
	}
	if creds.Env != "" {
		sourceCount++
	}
	if len(creds.Command) > 0 {
		sourceCount++
	}
	switch {
	case sourceCount == 0:
		return nil, credentialsUnavailable(fmt.Errorf(
			"medium %q: exactly one of credentials.file, credentials.env or credentials.command is required; "+
				"there is no anonymous access and no ambient credential chain reachable through this adapter", medium.ID))
	case sourceCount > 1:
		return nil, credentialsUnavailable(fmt.Errorf(
			"medium %q: exactly one of credentials.file, credentials.env or credentials.command may be set, not more than one", medium.ID))
	}

	cfg := configmap.Simple{}

	if creds.File != "" {
		if err := checkCredentialsFileCustody(medium.ID, creds.File); err != nil {
			return nil, err
		}
		// env_auth=true is the AWS credential CHAIN, not just this file,
		// and two of its other links can silently outrank the file: an
		// ambient AWS_* variable, and the instance-metadata fallback a
		// file with no [default] profile falls through to. Both are
		// refused here, before rclone is handed anything. See each
		// function's doc for the measurement behind it.
		if err := refuseAmbientAWSCredentialEnvironment(medium.ID); err != nil {
			return nil, err
		}
		if err := checkCredentialsFileHasADefaultProfile(medium.ID, creds.File); err != nil {
			return nil, err
		}
		// The preferred path. rclone's s3 backend passes
		// shared_credentials_file to the AWS SDK's own shared-config
		// loader, which is only consulted when env_auth is set and no
		// static key is configured (backend/s3/s3.go), so both options
		// are needed and neither is optional.
		//
		// This adapter never PARSES the file and never handles a value
		// out of it: the profile check above scans its headers and
		// nothing else, and rclone opens it itself for the credentials.
		// See checkCredentialsFileHasADefaultProfile for exactly what
		// that scan reads and what it does not claim.
		cfg.Set("env_auth", "true")
		cfg.Set("shared_credentials_file", creds.File)
		return cfg, nil
	}

	var resolved resolvedCredentials
	var err error
	switch {
	case creds.Env != "":
		resolved, err = resolveCredentialsFromEnv(creds.Env)
		if err != nil {
			err = fmt.Errorf("medium %q: resolving credentials from environment variable %q: %w", medium.ID, creds.Env, err)
		}
	default:
		resolved, err = resolveCredentialsFromCommand(creds.Command)
		if err != nil {
			err = fmt.Errorf("medium %q: resolving credentials from the configured command: %w", medium.ID, err)
		}
	}
	if err != nil {
		return nil, credentialsUnavailable(err)
	}

	// env_auth stays false here: a static key is configured, and rclone
	// only consults the SDK's ambient chain when there is none. Saying so
	// explicitly rather than relying on the option's zero value is the
	// difference between "this adapter decided" and "nobody decided".
	cfg.Set("env_auth", "false")
	cfg.Set("access_key_id", resolved.accessKeyID.Reveal())
	cfg.Set("secret_access_key", resolved.secretAccessKey.Reveal())
	if resolved.hasSessionToken {
		cfg.Set("session_token", resolved.sessionToken.Reveal())
	}
	return cfg, nil
}

// checkCredentialsFileCustody refuses a credentials file this manager
// would be reckless to hand to rclone: one that is missing, one that is
// not a regular file, one any other local account can read, or one sitting
// somewhere any other local account can replace it.
//
// It is the same reasoning #293 and #311 applied to key_file, with one
// deliberate difference. That check demands EXACTLY 0600, because
// importSSHKeyInto wrote the key with that mode and any drift from it is
// evidence of tampering. There is no import flow for medium credentials
// yet (that is FR-33's API half, and it is not in this issue), so an
// operator writes this file by hand and 0400 is a perfectly good answer.
// What matters either way is that nobody else can read it, so that is what
// this checks rather than an exact mode a hand-written file has no reason
// to match.
func checkCredentialsFileCustody(mediumID, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return credentialsUnavailable(fmt.Errorf(
			"medium %q: credentials.file %q is not accessible: %w", mediumID, path, err))
	}
	if info.IsDir() {
		return credentialsUnavailable(fmt.Errorf(
			"medium %q: credentials.file %q is a directory, not a file", mediumID, path))
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return credentialsUnavailable(fmt.Errorf(
			"medium %q: credentials.file %q has permissions %04o, which lets an account other than its owner read it: "+
				"an S3 credential unlocks every retained artifact on the medium at once; correct it (chmod go-rwx %s)",
			mediumID, path, mode, path))
	}
	if dir, mode, err := firstWritableAncestor(path); err != nil {
		return credentialsUnavailable(fmt.Errorf(
			"medium %q: checking the directories containing credentials.file %q: %w", mediumID, path, err))
	} else if dir != "" {
		return credentialsUnavailable(fmt.Errorf(
			"medium %q: credentials.file %q has a containing directory %q with permissions %04o: a group- or world-writable "+
				"directory lets any local actor delete or replace the file regardless of its own mode; "+
				"correct it (chmod go-w %s) or move the file",
			mediumID, path, dir, mode.Perm(), dir))
	}
	return nil
}

// ambientAWSCredentialEnvVars are the environment variables that can take
// this process's S3 authentication away from the configured medium.
//
// # Why this list exists at all
//
// The `file` source is the preferred one because rclone opens the file
// itself and the secret never enters this process. The only way to make
// rclone open one is env_auth=true, and env_auth=true sends it to the AWS
// SDK's LoadDefaultConfig (backend/s3/s3.go:1514-1525). That is a CHAIN,
// and the configured file is not the first link in it. An AWS_ACCESS_KEY_ID
// sitting in this process's environment wins over the file silently, and
// the backup then runs as an account nobody chose, writing artifacts
// somewhere nobody will look for them.
//
// It is ssh-agent's failure mode in different clothes, so it gets
// ssh-agent's answer: refuse, name what to unset, and let a person decide.
// Each entry is here with a reason, and the subtle one is worth reading:
//
//   - AWS_ACCESS_KEY_ID / AWS_ACCESS_KEY, AWS_SECRET_ACCESS_KEY /
//     AWS_SECRET_KEY, AWS_SESSION_TOKEN: the SDK's environment provider
//     sits AHEAD of the shared-config provider, so these simply win.
//   - AWS_PROFILE / AWS_DEFAULT_PROFILE: selects which profile is read out
//     of the file, and this adapter has no profile setting to state the
//     one it meant.
//   - AWS_SHARED_CREDENTIALS_FILE: the subtle one, and the reason this
//     list is not just "the two key variables". rclone's
//     shared_credentials_file option is passed through
//     awsconfig.WithSharedConfigFiles, which sets the SDK's *config* file
//     list (config@v1.32.30 load_options.go:475). AWS_SHARED_CREDENTIALS_FILE
//     populates a SEPARATE *credentials* file list, and
//     LoadSharedConfigProfile merges the credentials sections OVER the
//     config sections (shared_config.go:694), so for the same profile the
//     credentials file wins. rclone's option name says "credentials" and
//     the SDK reads it as "config"; that mismatch is invisible from here,
//     and it is why the whole list gets refused rather than the obvious
//     two.
//   - AWS_CONFIG_FILE: WithSharedConfigFiles is expected to override this,
//     but "expected to" is not the standard this file holds itself to for
//     a variable that selects where credentials are read from.
//   - AWS_WEB_IDENTITY_TOKEN_FILE / AWS_ROLE_ARN,
//     AWS_CONTAINER_CREDENTIALS_RELATIVE_URI /
//     AWS_CONTAINER_CREDENTIALS_FULL_URI: further links in the same chain,
//     each of which can authenticate as somebody else entirely.
//
// It applies to the `file` source ONLY. env and command set a static key,
// and rclone consults the SDK's chain only when there is none
// (s3.go:1514's `opt.AccessKeyID == "" && opt.SecretAccessKey == ""`), so
// for those two the ambient environment is already unreachable.
var ambientAWSCredentialEnvVars = []string{
	"AWS_ACCESS_KEY_ID",
	"AWS_ACCESS_KEY",
	"AWS_SECRET_ACCESS_KEY",
	"AWS_SECRET_KEY",
	"AWS_SESSION_TOKEN",
	"AWS_PROFILE",
	"AWS_DEFAULT_PROFILE",
	"AWS_SHARED_CREDENTIALS_FILE",
	"AWS_CONFIG_FILE",
	"AWS_WEB_IDENTITY_TOKEN_FILE",
	"AWS_ROLE_ARN",
	"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI",
	"AWS_CONTAINER_CREDENTIALS_FULL_URI",
}

// refuseAmbientAWSCredentialEnvironment refuses when this process's own
// environment carries anything that could outrank the configured
// credentials file.
//
// Set-but-empty counts as unset, which is how the SDK treats it: an empty
// AWS_ACCESS_KEY_ID does not authenticate anything, and refusing one would
// refuse a perfectly ordinary way of clearing a variable in a unit file.
//
// The variable NAMES are reported, because a name is the whole content of
// the fix. No value ever is: one of these holds a secret key, and this
// refusal must not be the thing that prints it.
func refuseAmbientAWSCredentialEnvironment(mediumID string) error {
	var present []string
	for _, name := range ambientAWSCredentialEnvVars {
		if os.Getenv(name) != "" {
			present = append(present, name)
		}
	}
	if len(present) == 0 {
		return nil
	}
	return credentialsUnavailable(fmt.Errorf(
		"medium %q uses credentials.file, which rclone reads through the AWS credential CHAIN, and this process's environment "+
			"carries %v, which the chain consults BEFORE the configured file. The backup would run as whichever account those name. "+
			"Unset them for this process, or switch the medium to credentials.env or credentials.command, which set a static key "+
			"the chain is never asked about",
		mediumID, present))
}

// checkCredentialsFileHasADefaultProfile refuses a credentials file that
// carries no profile named exactly `default`.
//
// # Why this exists, and it is not a style rule
//
// A file whose only profile is `[cold-storage]` does not FAIL. It HANGS.
// The SDK's chain finds no profile it was asked for, keeps walking, and
// the last link is EC2 instance metadata; on a host that is not an EC2
// instance that is a connection to the link-local 169.254.169.254 which
// nothing answers, so the operation stalls for as long as the caller
// allows. Timed against a real MinIO, with a 12-second deadline:
//
//	default.creds     took=7ms       err=<nil>
//	named.creds       took=12.002s   err=... context deadline exceeded
//	profiled.creds    took=12s       err=... context deadline exceeded
//
// It would have been an hour against an hour. A deployment experiences
// that as "backups got slow", not as a typo in a profile name, which is
// the protection-dies-quietly failure this product exists to prevent.
// There is no rclone option and no SDK option reachable from here that
// shortens it: the SDK's own switch is the AWS_EC2_METADATA_DISABLED
// environment variable, and a library may not mutate its process's
// environment.
//
// # What this reads, and what it does not
//
// This is the ONE place this process opens a credentials file, so the
// property that makes `file` the preferred source deserves restating
// exactly rather than being quietly weakened.
//
// It reads the file's LINES to find its profile HEADERS. It keeps only the
// header names and compares them against one literal. It never parses a
// setting, never wraps a value, never returns one, and never hands one to
// rclone, which still opens the file itself for the actual credentials.
// TestCredentialsFileProfileCheckReadsNoValues plants a file full of
// settings and proves none of their names or values reaches the refusal.
//
// What it does NOT claim, in keysource.go's own words about the same
// problem: the bytes are not reliably erased. The scanner's initial buffer
// is zeroed here and a credentials file's lines are far shorter than it,
// so in practice that is the buffer the data passed through, but
// bufio.Scanner allocates a larger one if a line outgrows it and
// Scanner.Text returns a freshly allocated string per line that Go gives
// no supported way to overwrite. The honest statement is: a key's bytes
// pass through memory this function owns, for one scan of one small file,
// best-effort overwritten. That is strictly less exposure than env and
// command accept for their whole operation, and it buys a medium that
// fails instead of hanging.
//
// # Why `default` specifically, and not "any single profile"
//
// Because there is no profile field in transport.MediumCredentials to
// select with, and rclone passes the configured path through
// WithSharedConfigFiles, so the SDK looks for whichever profile it was
// told to use, which with nothing set is `default`. The env and command
// sources accept a single profile under any name because THIS package
// parses those itself and can take the one profile there is; the file
// source's selection happens inside the SDK, where this package has no
// say.
func checkCredentialsFileHasADefaultProfile(mediumID, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return credentialsUnavailable(fmt.Errorf(
			"medium %q: credentials.file %q could not be read: %w", mediumID, path, err))
	}
	defer f.Close()

	buf := make([]byte, 0, 4096)
	defer zeroBytes(buf[:cap(buf)])
	scanner := bufio.NewScanner(f)
	scanner.Buffer(buf, maxResolvedCredentialsSize)

	var profiles []string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "[") || !strings.HasSuffix(line, "]") {
			// Not a header. Nothing about this line is looked at, kept or
			// reported.
			continue
		}
		name := strings.TrimSpace(line[1 : len(line)-1])
		if name == "default" {
			return nil
		}
		profiles = append(profiles, name)
	}
	if err := scanner.Err(); err != nil {
		return credentialsUnavailable(fmt.Errorf(
			"medium %q: credentials.file %q could not be read as text", mediumID, path))
	}
	// The profile NAMES are reported, because a name is not a secret and is
	// the whole content of the fix. No value ever is.
	if len(profiles) == 0 {
		return credentialsUnavailable(fmt.Errorf(
			"medium %q: credentials.file %q has no [profile] header at all, so it must carry a [default] one: "+
				"there is no profile setting to select any other with", mediumID, path))
	}
	return credentialsUnavailable(fmt.Errorf(
		"medium %q: credentials.file %q declares %v but no [default] profile. Rename one to [default]: there is no profile "+
			"setting to select another with, and a file the AWS credential chain cannot resolve does not fail, it falls "+
			"through to EC2 instance metadata and stalls until the operation times out",
		mediumID, path, profiles))
}

// resolveCredentialsFromEnv reads name from the environment and validates
// it as shared-credentials text.
func resolveCredentialsFromEnv(name string) (resolvedCredentials, error) {
	val, ok := os.LookupEnv(name)
	if !ok {
		return resolvedCredentials{}, fmt.Errorf("environment variable %q is not set", name)
	}
	if len(val) > maxResolvedCredentialsSize {
		return resolvedCredentials{}, fmt.Errorf("value exceeds %d bytes, refusing to treat it as credentials", maxResolvedCredentialsSize)
	}
	// []byte(val) copies, for resolveKeyFromEnv's reason: val is backed by
	// Go's own copy of the process environment block, which there is no
	// supported way to zero, and buf is a copy this function owns outright.
	buf := []byte(val)
	defer zeroBytes(buf)
	return parseSharedCredentials(buf)
}

// resolveCredentialsFromCommand runs argv and treats its stdout as
// shared-credentials text.
//
// The exec discipline (never a shell, a bounded timeout, a fixed minimal
// environment, its own process group so the timeout reaches whatever it
// spawned, bounded and zeroed stdout) is not re-implemented here: it is
// runResolverCommand, which keysource.go's own resolveKeyFromCommand now
// calls too. One implementation, so a hardening applied to one resolver
// cannot fail to reach the other.
func resolveCredentialsFromCommand(argv []string) (resolvedCredentials, error) {
	stdout, err := runResolverCommand(argv, maxResolvedCredentialsSize)
	if stdout != nil {
		defer stdout.zero()
	}
	if err != nil {
		return resolvedCredentials{}, err
	}
	return parseSharedCredentials(stdout.buf.Bytes())
}

// parseSharedCredentials is the SHAPE validation every resolved credential
// goes through before it is wrapped and before rclone ever sees it.
//
// It is deliberately a small, strict reader rather than a general INI
// parser. What it accepts is what an AWS shared-credentials file says and
// nothing else, so a secrets manager answering with an error string, an
// HTML login page, a JSON blob or an empty body fails HERE, at the point
// the bytes were produced, instead of surfacing much later as a confusing
// AccessDenied against a request nobody can trace back to a bad resolver.
//
// raw is never quoted back in an error, for keysource.go's reason.
func parseSharedCredentials(raw []byte) (resolvedCredentials, error) {
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return resolvedCredentials{}, errCredentialsEmpty
	}

	profiles := map[string]map[string]string{}
	order := []string{}
	current := ""
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			current = strings.TrimSpace(line[1 : len(line)-1])
			if current == "" {
				return resolvedCredentials{}, errCredentialsUnreadable
			}
			if _, seen := profiles[current]; !seen {
				profiles[current] = map[string]string{}
				order = append(order, current)
			}
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found || current == "" {
			// A key before any profile header, or a line that is not a
			// key/value pair at all, means this is not credentials text.
			return resolvedCredentials{}, errCredentialsUnreadable
		}
		profiles[current][strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}

	var chosen map[string]string
	switch {
	case len(order) == 0:
		return resolvedCredentials{}, errCredentialsUnreadable
	case profiles["default"] != nil:
		chosen = profiles["default"]
	case len(order) == 1:
		chosen = profiles[order[0]]
	default:
		// Several profiles and no `default`: this adapter exposes no
		// profile setting, so picking one would be guessing which
		// account an operator meant to back up with.
		return resolvedCredentials{}, errCredentialsAmbiguousProfile
	}

	id := chosen["aws_access_key_id"]
	secret := chosen["aws_secret_access_key"]
	if id == "" || secret == "" {
		return resolvedCredentials{}, errCredentialsIncomplete
	}
	if strings.ContainsAny(id, " \t") || strings.ContainsAny(secret, " \t") {
		// Neither an access key id nor a secret access key contains
		// whitespace, so whitespace inside one means the text was quoted,
		// wrapped or truncated on its way here.
		return resolvedCredentials{}, errCredentialsMalformedValue
	}

	out := resolvedCredentials{
		accessKeyID:     obs.NewSecret(id),
		secretAccessKey: obs.NewSecret(secret),
	}
	if token := chosen["aws_session_token"]; token != "" {
		out.sessionToken = obs.NewSecret(token)
		out.hasSessionToken = true
	}
	return out, nil
}

// The refusals parseSharedCredentials can make. Values rather than
// formatted strings, so a test can assert WHICH rule fired without
// matching prose, and so none of them can accidentally grow a %v that
// interpolates the bytes that failed.
var (
	errCredentialsEmpty            = credentialsError("resolved credentials are empty")
	errCredentialsUnreadable       = credentialsError("resolved value is not AWS shared-credentials text (expected a [profile] header and aws_access_key_id / aws_secret_access_key lines)")
	errCredentialsAmbiguousProfile = credentialsError("resolved credentials name several profiles and none of them is [default], and this adapter has no profile setting to disambiguate with")
	errCredentialsIncomplete       = credentialsError("resolved credentials are missing aws_access_key_id or aws_secret_access_key")
	errCredentialsMalformedValue   = credentialsError("a resolved credential value contains whitespace, so the text was quoted, wrapped or truncated on its way here")
)

// credentialsError is a named string type rather than errors.New, so the
// five values above cannot be confused with any other error in this
// package by an equality check that meant to compare something else. It is
// transport/medium.go's mediumKeyError applied to the same problem, kept
// local for the same reason: a shared error type across two packages would
// make two unrelated refusals compare equal.
type credentialsError string

func (e credentialsError) Error() string { return string(e) }
