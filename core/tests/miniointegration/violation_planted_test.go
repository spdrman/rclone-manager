//go:build canaryviolation

package miniointegration_test

import (
	"log"

	"github.com/spdrman/rclone-manager/core/tests/miniofixture"
)

// plantCanaryViolation is FR-33's planted violation: a build that logs the
// resolved medium configuration verbatim, which is the single most likely
// way this product would ever leak an S3 credential in real life. Somebody
// debugging a medium that will not authenticate adds one line printing the
// config they just built, it survives review because it looks like every
// other debug line, and from then on every support bundle carries the key.
//
// Built with -tags canaryviolation, TestMediumCredentialCanary must FAIL,
// on all three credential sources. If it ever passes under this tag, the
// canary is not looking where it claims to look and the guard is theatre.
//
//	cd core && GOWORK=off go test -tags canaryviolation \
//	    ./tests/miniointegration/ -run TestMediumCredentialCanary
//
// The recorded failing run is in the landing PR.
func plantCanaryViolation(f *miniofixture.Fixture) {
	log.Printf("medium credentials resolved: access_key_id=%s secret_access_key=%s",
		f.AccessKeyID, f.SecretAccessKey)
}
