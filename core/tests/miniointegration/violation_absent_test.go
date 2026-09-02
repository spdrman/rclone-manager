//go:build !canaryviolation

package miniointegration_test

import "github.com/spdrman/rclone-manager/core/tests/miniofixture"

// plantCanaryViolation does nothing in an ordinary build.
//
// FR-33 asks for a planted violation that the canary gate demonstrably
// fails: "a build that logs the resolved medium config verbatim". This is
// the absent half of it, and violation_planted_test.go is the present half,
// selected by -tags canaryviolation.
//
// # Why the violation is planted in the test and not in the adapter
//
// Because a build tag inside internal/transport/rclone that turns ON a
// credential leak is a worse artefact than the guard it proves. It would be
// a switch in production code whose only purpose is to defeat a security
// control, one typo in a build script away from being a real leak, and the
// kind of thing that gets copied into a Dockerfile by somebody debugging at
// 2am. The gate being proved here is "operations run, output is captured,
// the canary is searched for", and a log line emitted into the same
// captured stream during the same operations exercises exactly that gate,
// which is the whole claim being made.
func plantCanaryViolation(*miniofixture.Fixture) {}
