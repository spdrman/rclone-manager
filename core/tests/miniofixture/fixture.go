// Package miniofixture is what is left of the MinIO fixture after #450
// folded its body into core/tests/machines: two lines of forwarding, and
// nothing else.
//
// It exists for exactly one caller, core/tests/conformance, which other
// lanes are editing at the same time as this change. Converting it here
// would have meant rewriting files being rewritten, so the conversion is
// the last step of #450 rather than part of it, and it is two edits per
// file:
//
//	miniofixture.Start(t)  ->  machines.Start(t).Medium(t)
//	*miniofixture.Fixture  ->  *machines.Medium
//
// Nothing else changes: Medium, MediumForBucket, NewBucket, Endpoint,
// Bucket, Region, the credentials and ContainerID all keep their names on
// machines.Medium, which is why Fixture below can be a plain alias.
//
// Once core/tests/conformance is on core/tests/machines this directory
// goes, and "tests/miniofixture" comes off core/internal/testtier's
// HarnessDirs with it. Nothing here execs docker or holds any behaviour of
// its own, so until then it is a name and not a second harness.
package miniofixture

import (
	"testing"

	"github.com/spdrman/rclone-manager/core/tests/machines"
)

// Fixture is machines.Medium under the name its remaining callers know.
type Fixture = machines.Medium

// Start brings up a medium on a dedicated network, the way every other
// machine-tier test now gets one.
func Start(t *testing.T) *Fixture {
	t.Helper()
	return machines.Start(t).Medium(t)
}
