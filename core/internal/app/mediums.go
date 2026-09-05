package app

import (
	"fmt"
	"strings"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// The production placement.MediumResolver (EPIC E, FR-27 and FR-31; issue
// #239, inherited from #238's acceptance line).
//
// It lives here rather than in internal/placement because
// transport.Medium's own doc already named this package as the one that
// builds one: "internal/app builds one from a config.StorageMedium;
// nothing below this line reads config." internal/placement sits below
// that line for the fields it needs and does not read a medium
// declaration; this package is where a configured place becomes a
// reachable one.

// MediumResolver turns a deployment's declared storage mediums into the
// two things the move engine needs about one: how to reach it, and what a
// copy on it has to ACHIEVE before a source may be deleted.
//
// It is a slice rather than a map, and it is looked up linearly, because
// the list is the config file's own list: a deployment has a handful of
// mediums, the order is the operator's, and a map built here would be a
// second index that can go stale against a hot reload.
type MediumResolver []config.StorageMedium

// Resolve implements placement.MediumResolver.
//
// # Why the class comes back beside the medium
//
// #238's seam returns the required placement.Class rather than the raw
// upload_verification string, and that is what makes FR-31's "attested
// fails loudly" structural rather than careful. placement.Verify never
// falls back, so a medium whose required class is Attested against an
// endpoint that cannot attest produces ErrClassUnavailable and the move
// refuses. Measured against rclone v1.75.0, no s3 medium can attest at all.
//
// An operator no longer reaches that refusal, and the reason is worth
// knowing before anyone concludes this branch is dead code.
// config.validateUploadVerificationIsAchievable refuses
// `upload_verification: attested` on an s3 medium at load, so the
// configuration never starts, and the older reading of this paragraph, that
// the operator finds out at the point a move would otherwise have deleted a
// local copy, described the product before that check existed. Refusing at
// load is strictly better: the same answer, hours earlier, without a cycle
// having to reach a medium to produce it.
//
// The class returned here is the second of the two, not a duplicate of it.
// The load-time check reads a table of which TYPES can attest, which is a
// static claim; this one puts the requirement in front of a verification
// that queries the endpoint's real capability at the moment of the move. A
// medium type added to that table on the strength of a backend that turns
// out not to serve the digest still refuses here, before a source copy is
// deleted, which is where the guarantee has to hold.
//
// Existence is deliberately unreachable from any configuration. It proves
// only that an object is there at the recorded size, and a source copy
// deleted on the strength of it is a backup deleted against a check that
// never looked at the bytes.
func (r MediumResolver) Resolve(id string) (transport.Medium, placement.Class, error) {
	if id == "" {
		return transport.Medium{}, "", fmt.Errorf("app: no medium id was given, so there is nothing to resolve")
	}
	if id == config.MediumLocal {
		// Reserved in both directions (config.MediumLocal's own doc). A
		// local placement is a backup set's own local_path, reached
		// through internal/artifactstore, and there is no transport.Medium
		// that describes one. Answering with a zero Medium would hand the
		// caller a place with no bucket and no endpoint.
		return transport.Medium{}, "", fmt.Errorf(
			"app: %q is the implicit local medium (a backup set's own local_path), not a configured destination; it is reached through the local store, never through a medium",
			config.MediumLocal)
	}

	for _, m := range r {
		if m.ID != id {
			continue
		}
		typ, err := mediumType(m)
		if err != nil {
			return transport.Medium{}, "", err
		}
		class, err := requiredClass(m)
		if err != nil {
			return transport.Medium{}, "", err
		}
		return transport.Medium{
			ID:           m.ID,
			Type:         typ,
			Region:       m.Region,
			Endpoint:     m.Endpoint,
			Bucket:       m.Bucket,
			Prefix:       m.Prefix,
			StorageClass: m.EffectiveStorageClass(),
			Credentials: transport.MediumCredentials{
				File:    m.Credentials.File,
				Env:     m.Credentials.Env,
				Command: append([]string(nil), m.Credentials.Command...),
			},
		}, class, nil
	}
	return transport.Medium{}, "", fmt.Errorf("app: no storage medium %q is declared; this deployment declares %s", id, r.names())
}

// mediumType maps the schema's closed type set onto the transport's own.
// The two are the same strings by construction (a test in internal/
// transport pins that), and this is still a switch rather than a cast,
// because a type this build cannot reach must be a refusal at the moment
// something is about to be reached rather than a value handed onward.
func mediumType(m config.StorageMedium) (transport.MediumType, error) {
	switch m.Type {
	case config.StorageMediumTypeS3:
		return transport.MediumTypeS3, nil
	default:
		return "", fmt.Errorf("app: storage medium %q declares type %q, which this build has no backend for", m.ID, m.Type)
	}
}

// requiredClass is FR-31's mapping, and the one place it is written.
func requiredClass(m config.StorageMedium) (placement.Class, error) {
	switch v := m.EffectiveUploadVerification(); v {
	case config.UploadVerificationReadback:
		return placement.Content, nil
	case config.UploadVerificationAttested:
		return placement.Attested, nil
	default:
		return "", fmt.Errorf(
			"app: storage medium %q declares upload_verification %q, which this build does not understand; "+
				"refusing rather than falling back to a weaker check, because what follows a believed upload is deleting the local copy",
			m.ID, v)
	}
}

// names renders the declared ids for the refusal above, so the message
// says what WAS available rather than only what was not.
func (r MediumResolver) names() string {
	if len(r) == 0 {
		return "none"
	}
	ids := make([]string, 0, len(r))
	for _, m := range r {
		ids = append(ids, m.ID)
	}
	return strings.Join(ids, ", ")
}
