// The keyless signing identity, and the command a verifier runs against it.
//
// Sigstore keyless signing binds a signature to the Subject Alternative
// Name of the short-lived Fulcio certificate the release workflow gets in
// exchange for its GitHub OIDC token. GitHub builds that SAN as
//
//	<repository URL>/<workflow path>@<the ref the run was triggered on>
//
// so the ref half of it is decided by the workflow's trigger and by
// nothing else. That is the whole reason these constants and
// .github/workflows/release.yml cannot be allowed to drift apart:
// signing.identity in the provenance bundle, and the command the
// compliance docs print from it, are only true while they name the ref
// the workflow actually runs on.
//
// Issue #510. This used to record @refs/tags/*, and the printed command
// pinned a regexp anchored on @refs/tags/. The workflow has never run on
// a tag: it publishes on a push to the release branch. Run against the
// published 0.2.0, the documented command failed with
//
//	Error: no matching signatures: none of the expected identities matched
//	what was in the certificate, got subjects
//	[https://github.com/spdrman/rclone-manager/.github/workflows/release.yml@refs/heads/release]
//	with issuer https://token.actions.githubusercontent.com
//
// and the certificate on that signature carries
// githubWorkflowRef=refs/heads/release. That is the most damaging thing a
// compliance record can do. It is not "verification is unavailable": it
// is a real, correctly signed release reported as unverifiable to
// somebody who followed our own instructions, whose reasonable conclusion
// is that the artifact was forged. The same command with the identity
// below passes against the same image.
//
// TestSigningIdentityMatchesTheWorkflowTriggerThatPublishes reads the
// workflow and refuses any disagreement between its trigger and these
// constants, so moving one forces the other.
package packaging

import "strings"

const (
	// SigningWorkflowPath is the workflow whose OIDC identity signs a
	// release, relative to the repository root. It is half of the
	// certificate SAN, so it is a path that has to keep its name.
	SigningWorkflowPath = ".github/workflows/release.yml"

	// SigningWorkflowBranch is the branch a push to which publishes, and
	// therefore the ref the signing certificate is issued against. See
	// docs/release-branch.md for why `release` is the only branch that
	// can be it.
	SigningWorkflowBranch = "release"

	// SigningRepositoryURL is the repository the workflow lives in. It
	// repeats sourceRepository.url from compliance.json rather than
	// reading it, because this is a security contract that should be
	// readable in one place, and the test holds the two to each other.
	SigningRepositoryURL = "https://github.com/spdrman/rclone-manager"

	// SigningCertificateIssuer is the OIDC issuer a verifier pins
	// alongside the identity. Pinning the identity without the issuer
	// would accept the same-looking subject from any issuer Fulcio
	// trusts.
	SigningCertificateIssuer = "https://token.actions.githubusercontent.com"

	// SigningIdentity is the exact certificate SAN a release carries.
	//
	// Exact, not a regexp. The ref is a single fixed branch, so there is
	// nothing to match loosely, and an exact identity cannot be widened
	// by a missing anchor: the regexp this replaced ended at
	// `@refs/tags/` with no `$`, which would have accepted every tag ref
	// in the repository had any of them ever signed anything.
	SigningIdentity = SigningRepositoryURL + "/" + SigningWorkflowPath + "@refs/heads/" + SigningWorkflowBranch
)

// SigningVerifyCommand is the command that verifies a published
// reference, as one line, so the provenance bundle and the compliance
// docs cannot print two different commands.
func SigningVerifyCommand(reference string) string {
	return strings.Join([]string{
		"cosign verify",
		"--certificate-oidc-issuer", SigningCertificateIssuer,
		"--certificate-identity", SigningIdentity,
		reference,
	}, " ")
}
