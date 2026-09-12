package secretref

import (
	"context"
	"fmt"
	"strings"

	"github.com/backupdproject/backupd/core/internal/obs"
)

// AWSCredentials is one resolved object-store credential set on its way
// into a storage provider's options, and nowhere else.
//
// SecretAccessKey and SessionToken are obs.Secret. AccessKeyID is not, and
// that is a decision rather than an oversight: it identifies the principal
// and appears in the provider's own access logs, so it is the one field an
// operator can be shown when they need to know WHICH key a repository is
// using, and refusing to render it would make a misconfigured key
// impossible to diagnose without a debugger.
//
// The type deliberately has no String, Format or MarshalJSON of its own.
// See the leak test: obs.Secret does not protect a value in an unexported
// field, but every field here is exported, so fmt, encoding/json and
// log/slog all reach the wrapper's own redaction. A future unexported
// field would break that silently, which is why there are none.
type AWSCredentials struct {
	// AccessKeyID is the key id. Not secret; see the type doc.
	AccessKeyID string

	// SecretAccessKey is the secret half.
	SecretAccessKey obs.Secret

	// SessionToken is set only for temporary credentials, and HasSession
	// says whether it was declared: an empty token and an absent one are
	// different requests to make of a provider.
	SessionToken obs.Secret

	// HasSession reports whether SessionToken was present in the resolved
	// material.
	HasSession bool
}

// The refusals ParseSharedCredentials can make.
//
// Values rather than formatted strings, so a test can assert WHICH rule
// fired without matching prose, and so none of them can grow a %v that
// interpolates the bytes that failed.
var (
	// ErrCredentialsUnreadable means the resolved bytes are not AWS
	// shared-credentials text at all. This is the one that catches a
	// secrets manager answering with an error string, an HTML login page
	// or a JSON blob, at the point the bytes were produced rather than
	// later as an unattributable AccessDenied.
	ErrCredentialsUnreadable = credentialsError("resolved value is not AWS shared-credentials text (expected a [profile] header and aws_access_key_id / aws_secret_access_key lines)")

	// ErrCredentialsAmbiguousProfile means several profiles were declared
	// and none is [default]. Picking one would be guessing which account
	// an operator meant to back up with.
	ErrCredentialsAmbiguousProfile = credentialsError("resolved credentials name several profiles and none of them is [default], and there is no profile setting to disambiguate with")

	// ErrCredentialsIncomplete means the chosen profile is missing one of
	// the two required keys.
	ErrCredentialsIncomplete = credentialsError("resolved credentials are missing aws_access_key_id or aws_secret_access_key")

	// ErrCredentialsMalformedValue means a value contains whitespace, so
	// the text was quoted, wrapped or truncated on its way here. Neither
	// an access key id nor a secret access key contains whitespace.
	ErrCredentialsMalformedValue = credentialsError("a resolved credential value contains whitespace, so the text was quoted, wrapped or truncated on its way here")
)

// credentialsError is a named string type rather than errors.New, so the
// four values above cannot be confused with any other error by an equality
// check that meant to compare something else.
type credentialsError string

func (e credentialsError) Error() string { return string(e) }

// ResolveAWS resolves a reference whose material is AWS
// shared-credentials text.
//
// The format is not a choice this package made: it is what an operator
// already points a storage medium at (config.MediumCredentials.File is a
// shared-credentials file, because that is what the medium plane's backend
// reads), so a repository on the same bucket resolves the same file rather
// than asking for the same secret again in a second format.
func ResolveAWS(ctx context.Context, ref Ref) (AWSCredentials, error) {
	raw, err := resolve(ctx, ref)
	if raw != nil {
		defer zero(raw)
	}

	if err != nil {
		return AWSCredentials{}, err
	}

	creds, err := ParseSharedCredentials(raw)
	if err != nil {
		// The source, never the material: which file or variable to go
		// and look at is the entire actionable content of this failure.
		return AWSCredentials{}, fmt.Errorf("secretref: %s: %w", ref, err)
	}

	return creds, nil
}

// ParseSharedCredentials is the SHAPE validation resolved object-store
// credentials go through before any provider sees them.
//
// It is deliberately a small, strict reader rather than a general INI
// parser. What it accepts is what an AWS shared-credentials file says and
// nothing else, because the point is to fail on text that is not
// credentials at all, and a permissive parser succeeds on a surprising
// amount of that.
//
// raw is never quoted back in an error.
func ParseSharedCredentials(raw []byte) (AWSCredentials, error) {
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return AWSCredentials{}, ErrCredentialsUnreadable
	}

	profiles := map[string]map[string]string{}
	order := []string{}
	current := ""

	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}

		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			current = strings.TrimSpace(line[1 : len(line)-1])
			if current == "" {
				return AWSCredentials{}, ErrCredentialsUnreadable
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
			return AWSCredentials{}, ErrCredentialsUnreadable
		}

		profiles[current][strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}

	var chosen map[string]string

	switch {
	case len(order) == 0:
		return AWSCredentials{}, ErrCredentialsUnreadable
	case profiles["default"] != nil:
		chosen = profiles["default"]
	case len(order) == 1:
		chosen = profiles[order[0]]
	default:
		return AWSCredentials{}, ErrCredentialsAmbiguousProfile
	}

	id := chosen["aws_access_key_id"]
	secret := chosen["aws_secret_access_key"]

	if id == "" || secret == "" {
		return AWSCredentials{}, ErrCredentialsIncomplete
	}

	if strings.ContainsAny(id, " \t") || strings.ContainsAny(secret, " \t") {
		return AWSCredentials{}, ErrCredentialsMalformedValue
	}

	out := AWSCredentials{
		AccessKeyID:     id,
		SecretAccessKey: obs.NewSecret(secret),
	}

	if token := chosen["aws_session_token"]; token != "" {
		out.SessionToken = obs.NewSecret(token)
		out.HasSession = true
	}

	return out, nil
}
