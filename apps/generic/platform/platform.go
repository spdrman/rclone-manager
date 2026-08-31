// Package platform is the generic Docker/Linux provider's
// capabilities.PlatformAdapter (docs/EPIC-B-multi-nas.md §3.4).
//
// Since issue #167 it declares nothing of its own. The generic profile is
// one row of the runtime-profile table in
// apps/common/platform/profile, because runtime profile selection means
// one binary carries every profile and a table split across per-platform
// packages would reintroduce the per-platform build section 3.7 forbids.
// What is left here is the adapter this provider's own binary reaches for
// by name, delegating to that row: one declaration of what "generic"
// means, not two that can drift.
package platform

import (
	"context"

	"github.com/spdrman/rclone-manager/apps/common/auth/local"
	"github.com/spdrman/rclone-manager/apps/common/platform/capabilities"
	"github.com/spdrman/rclone-manager/apps/common/platform/profile"
)

// Adapter is the generic provider's capabilities.PlatformAdapter,
// backed by a local.Service for authentication.
type Adapter struct {
	inner capabilities.PlatformAdapter
}

// New returns a generic Adapter authenticating through auth.
//
// It panics on a wiring error rather than returning one, and that is a
// deliberate narrowing of profile.Profile.Adapter's contract: the generic
// profile has no gateway, so the single way that call can fail is a nil
// authenticator, which is a programming error at this call site and not
// a runtime condition an operator can produce. A caller that needs the
// error (a binary selecting a profile from a flag) should use
// profile.Lookup and profile.Profile.Adapter directly, as
// cmd/backup-manager-web does.
func New(auth *local.Service) Adapter {
	inner, err := profile.Generic.
		Profile().
		Adapter(profile.AdapterConfig{LocalAuth: auth.Authenticator()})
	if err != nil {
		panic("apps/generic/platform: the generic profile refused to wire: " + err.Error())
	}
	return Adapter{inner: inner}
}

func (a Adapter) ID() capabilities.PlatformID { return a.inner.ID() }

// Capabilities reports every capability as unsupported: the generic host
// has no native platform integration at all, only local authentication
// (which is not "native" in this contract's sense - it is this
// package's own emulation-free fallback, not a platform-provided
// session).
func (a Adapter) Capabilities() capabilities.PlatformCapabilities { return a.inner.Capabilities() }

// Authenticator returns the local-auth Authenticator apps/common/webhost's
// authMiddleware consults for every /api/v1 request.
func (a Adapter) Authenticator() capabilities.Authenticator { return a.inner.Authenticator() }

// Notifier returns the null-object notifier: this platform declares no
// native notification capability, and §22 forbids emulating one.
func (a Adapter) Notifier() capabilities.Notifier { return a.inner.Notifier() }

func (a Adapter) PlatformInfo(ctx context.Context) (capabilities.PlatformInfo, error) {
	return a.inner.PlatformInfo(ctx)
}
