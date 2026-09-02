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
//     env_auth + shared_credentials_file options), so the secret never
//     enters this process's memory at all, and a secret this process never
//     holds is a secret it cannot log, cannot leak through a %+v, and
//     cannot leave behind in a heap dump.
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
	"fmt"
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
		return nil, transport.NewError(transport.Configuration, "medium_credentials", fmt.Errorf(
			"medium %q: exactly one of credentials.file, credentials.env or credentials.command is required; "+
				"there is no anonymous access and no ambient credential chain reachable through this adapter", medium.ID))
	case sourceCount > 1:
		return nil, transport.NewError(transport.Configuration, "medium_credentials", fmt.Errorf(
			"medium %q: exactly one of credentials.file, credentials.env or credentials.command may be set, not more than one", medium.ID))
	}

	cfg := configmap.Simple{}

	if creds.File != "" {
		if err := checkCredentialsFileCustody(medium.ID, creds.File); err != nil {
			return nil, err
		}
		// The preferred path, and the whole reason it is preferred: this
		// adapter never opens the file. rclone's s3 backend passes
		// shared_credentials_file to the AWS SDK's own shared-config
		// loader, which is only consulted when env_auth is set and no
		// static key is configured (backend/s3/s3.go), so both options
		// are needed and neither is optional.
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
		return nil, transport.NewError(transport.Configuration, "medium_credentials", err)
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
		return transport.NewError(transport.Configuration, "medium_credentials", fmt.Errorf(
			"medium %q: credentials.file %q is not accessible: %w", mediumID, path, err))
	}
	if info.IsDir() {
		return transport.NewError(transport.Configuration, "medium_credentials", fmt.Errorf(
			"medium %q: credentials.file %q is a directory, not a file", mediumID, path))
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return transport.NewError(transport.Configuration, "medium_credentials", fmt.Errorf(
			"medium %q: credentials.file %q has permissions %04o, which lets an account other than its owner read it: "+
				"an S3 credential unlocks every retained artifact on the medium at once; correct it (chmod go-rwx %s)",
			mediumID, path, mode, path))
	}
	if dir, mode, err := firstWritableAncestor(path); err != nil {
		return transport.NewError(transport.Configuration, "medium_credentials", fmt.Errorf(
			"medium %q: checking the directories containing credentials.file %q: %w", mediumID, path, err))
	} else if dir != "" {
		return transport.NewError(transport.Configuration, "medium_credentials", fmt.Errorf(
			"medium %q: credentials.file %q has a containing directory %q with permissions %04o: a group- or world-writable "+
				"directory lets any local actor delete or replace the file regardless of its own mode; "+
				"correct it (chmod go-w %s) or move the file",
			mediumID, path, dir, mode.Perm(), dir))
	}
	return nil
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

type credentialsError string

func (e credentialsError) Error() string { return string(e) }
