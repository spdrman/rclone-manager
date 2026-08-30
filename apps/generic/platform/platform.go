// Package platform is the generic Docker/Linux provider's
// capabilities.PlatformAdapter (docs/EPIC-B-multi-nas.md §3.4): no native
// authentication, no native notifications, no storage picker, no
// embedded window, no app-store packaging - every capability this
// platform doesn't have is reported as false, never emulated (§22), the
// same all-false shape apps/generic/frontend/platform.ts's genericBridge
// already declares on the TypeScript side.
package platform

import (
	"context"

	"github.com/spdrman/rclone-manager/apps/common/auth/local"
	"github.com/spdrman/rclone-manager/apps/common/platform/capabilities"
)

// Adapter is the generic provider's capabilities.PlatformAdapter,
// backed by a local.Service for authentication.
type Adapter struct {
	capabilities.BasePlatformAdapter
	auth *local.Service
}

// New returns a generic Adapter authenticating through auth.
func New(auth *local.Service) Adapter {
	return Adapter{auth: auth}
}

func (a Adapter) ID() capabilities.PlatformID { return capabilities.PlatformGeneric }

// Capabilities reports every capability as unsupported: the generic host
// has no native platform integration at all, only local authentication
// (which is not "native" in this contract's sense - it is this
// package's own emulation-free fallback, not a platform-provided
// session).
func (a Adapter) Capabilities() capabilities.PlatformCapabilities {
	return capabilities.PlatformCapabilities{}
}

// Authenticator returns the local-auth Authenticator apps/common/webhost's
// authMiddleware consults for every /api/v1 request.
func (a Adapter) Authenticator() capabilities.Authenticator {
	return a.auth.Authenticator()
}

func (a Adapter) PlatformInfo(_ context.Context) (capabilities.PlatformInfo, error) {
	return capabilities.PlatformInfo{
		ID:         capabilities.PlatformGeneric,
		Name:       "Generic Docker / Linux",
		Deployment: "Docker Compose",
	}, nil
}
