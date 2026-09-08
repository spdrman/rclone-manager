// The import half of FR-33's credential story: turning material an
// operator has just been handed by their provider into the one thing this
// schema can hold, which is a REFERENCE (G2.2, issue #594).
//
// It sits beside mediumcreds.go rather than in core/service for the
// reason ValidateImportedPrivateKey sits beside keysource.go: the format
// a credentials FILE has to be in is decided by the AWS SDK rclone hands
// it to, and this package is the only one that knows what that SDK will
// accept. A second, friendlier idea of "shared-credentials text" written
// at the service layer would be a second format nobody could see the
// difference between until one of them failed in production, which is the
// exact trap mediumcreds.go's own "one format, three sources" note is
// about.
//
// Two entry points, because an operator arrives with the material in one
// of two shapes:
//
//   - RenderImportedMediumCredentials takes an access key id and a secret
//     access key, which is what a provider's console shows and what the
//     wizard's step 2 collects.
//   - ValidateImportedMediumCredentials takes shared-credentials TEXT,
//     which is what `backup-manager medium import-credentials --stdin`
//     reads and what an operator who already has a file will paste.
//
// Both end at the same bytes on disk, and both refuse by the SHAPE of the
// problem and never by quoting what failed. That rule is verbatim
// keysource.go's and it matters more here than anywhere: the caller of
// these functions is an HTTP handler and a CLI, so an error that quoted
// its input would put a secret in a response body and in a terminal
// transcript that is copy-to-clipboard and exportable.
package rclone

import (
	"fmt"
	"strings"
)

// requiredProfile is the one profile name an imported credentials file
// may carry.
//
// checkCredentialsFileHasADefaultProfile refuses a file without it at
// connection time, and it refuses for a reason worth repeating at import
// time rather than discovering later: a file the AWS credential chain
// cannot resolve does not fail, it falls through to EC2 instance metadata
// and stalls until the operation times out. Catching that here means an
// operator learns it while they still have the material in front of them.
const requiredProfile = "default"

// RenderImportedMediumCredentials turns an access key id and a secret
// access key into the AWS shared-credentials text this package's `file`
// source is handed to rclone unopened.
//
// The values are checked by shape first (present, and free of the
// whitespace that means the text was quoted, wrapped or truncated on its
// way here), which is exactly what parseSharedCredentials would refuse
// later, asked here so the refusal reaches the person who just pasted it.
// The result is then parsed back through parseSharedCredentials, so what
// this returns is bytes this package has already agreed it can read.
//
// sessionToken is optional and is "" for the ordinary long-lived key an
// operator gets from a provider console.
//
// Neither value is ever interpolated into an error. See this file's
// package doc.
func RenderImportedMediumCredentials(accessKeyID, secretAccessKey, sessionToken string) ([]byte, error) {
	if strings.TrimSpace(accessKeyID) == "" {
		return nil, credentialsError("an access key id is required")
	}
	if strings.TrimSpace(secretAccessKey) == "" {
		return nil, credentialsError("a secret access key is required")
	}
	for _, v := range []string{accessKeyID, secretAccessKey, sessionToken} {
		if strings.ContainsAny(v, " \t\r\n") {
			return nil, errCredentialsMalformedValue
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[%s]\n", requiredProfile)
	fmt.Fprintf(&b, "aws_access_key_id = %s\n", accessKeyID)
	fmt.Fprintf(&b, "aws_secret_access_key = %s\n", secretAccessKey)
	if sessionToken != "" {
		fmt.Fprintf(&b, "aws_session_token = %s\n", sessionToken)
	}
	raw := []byte(b.String())

	// Read back through this package's own parser rather than trusted
	// because it was just built here. The two could drift, and the
	// direction that drift would fail in is a file written by an import
	// that a connection then cannot use.
	if _, err := parseSharedCredentials(raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// ValidateImportedMediumCredentials checks raw as shared-credentials text
// that is about to be persisted as a medium's `file` source.
//
// It is parseSharedCredentials plus the [default] requirement, and the
// second half is the reason this is not simply parseSharedCredentials
// exported. That parser accepts a lone non-default profile, because it is
// also the parser for the `env` and `command` sources, whose text this
// process reads itself and hands to rclone as a static key: for those two
// the profile name never reaches the AWS SDK at all. A FILE is different.
// rclone opens it through env_auth, the SDK resolves a profile by name,
// and this adapter has no profile setting to name anything but default
// with. So a file is held to the stricter rule, here, at the moment it is
// written, rather than at the first backup that needs it.
//
// It returns nothing but an error on purpose: there is no half of this
// input that is safe to hand back to a caller.
func ValidateImportedMediumCredentials(raw []byte) error {
	if _, err := parseSharedCredentials(raw); err != nil {
		return err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "[") || !strings.HasSuffix(line, "]") {
			continue
		}
		if strings.TrimSpace(line[1:len(line)-1]) == requiredProfile {
			return nil
		}
	}
	// The profile NAMES are not reported here, unlike
	// checkCredentialsFileHasADefaultProfile, which reads a file an
	// administrator already put on the host. This reads material that
	// arrived on a request body or on stdin, and echoing any part of it
	// back is the thing this file exists not to do.
	return errCredentialsNoDefaultProfile
}

// errCredentialsNoDefaultProfile is ValidateImportedMediumCredentials'
// own refusal, a value beside mediumcreds.go's five for the same reason
// those are values: a test asserts WHICH rule fired without matching
// prose, and the string cannot grow a %v that interpolates what failed.
var errCredentialsNoDefaultProfile = credentialsError(
	"credentials text has no [default] profile; rename the profile you meant to [default], because this adapter has no " +
		"profile setting to select another with, and a file the AWS credential chain cannot resolve does not fail, it " +
		"falls through to EC2 instance metadata and stalls until the operation times out")
