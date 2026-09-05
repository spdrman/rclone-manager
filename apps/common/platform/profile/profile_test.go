package profile_test

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/apps/common/platform/capabilities"
	"github.com/spdrman/rclone-manager/apps/common/platform/profile"
)

// These tests hold the line that makes runtime profiles an adapter
// mechanism rather than a fork mechanism, and they are grouped by which
// half of that they defend.
//
// Selection covers the refusals: an unknown or empty selector must not
// fall back to generic, because a deployment that asked for a gateway
// profile and silently got local authentication is a security regression
// nobody would see. Inertness is the structural half, and it is a
// reflect-based allow-list rather than a marker scan because the scanner
// in scripts/architecture never visits this package: layers.conf files
// apps/common/platform under the core layer, and that scanner only reads
// the platform and distribution layers. Fail-closed wiring and the
// trusted-gateway boundary cover the two ways a wrong answer here becomes
// an unauthenticated or misattributed destructive API.
//
// Three of the assertions here carry an explicit positive control, and
// they are the three whose natural failure mode is passing. A checker that
// returns nil, an Adapter that refuses everything and a Sanitize that
// deletes every header would all satisfy their own tests; the controls are
// what make the green mean something.
//
// The package is profile_test on purpose. Everything here is reachable
// from outside, and a test with access to the unexported adapter struct
// could assert on a shape the callers who actually matter never see.

// ---------------------------------------------------------------------
// Selection
// ---------------------------------------------------------------------

func TestLookup(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		id         string
		wantID     profile.ID
		wantErr    error
		wantDetail string
	}{
		{name: "generic", id: "generic", wantID: profile.Generic},
		{name: "ugos", id: "ugos", wantID: profile.UGOS},
		{name: "truenas", id: "truenas", wantID: profile.TrueNAS},
		{name: "unraid", id: "unraid", wantID: profile.Unraid},
		{name: "openmediavault", id: "openmediavault", wantID: profile.OpenMediaVault},
		{name: "proxmox", id: "proxmox", wantID: profile.Proxmox},
		{name: "synology", id: "synology", wantID: profile.Synology},
		{
			// "synology" used to be the unknown token here, and issue
			// #169 made it a real profile. The refusal still has to be
			// exercised, so this is a token nothing will ever implement
			// rather than the next platform someone adds.
			name:       "unknown profile is refused, and the refusal names what is known",
			id:         "not-a-platform",
			wantErr:    profile.ErrUnknownProfile,
			wantDetail: "generic",
		},
		{
			name:    "an empty selector is refused rather than defaulted",
			id:      "",
			wantErr: profile.ErrUnknownProfile,
		},
		{
			name:    "case is not normalised away: a profile id is an exact token",
			id:      "Generic",
			wantErr: profile.ErrUnknownProfile,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := profile.Lookup(tc.id)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Lookup(%q) error = %v, want %v", tc.id, err, tc.wantErr)
				}
				if tc.wantDetail != "" && !strings.Contains(err.Error(), tc.wantDetail) {
					t.Errorf("Lookup(%q) error %q does not mention %q, so it does not say what IS valid", tc.id, err, tc.wantDetail)
				}
				return
			}
			if err != nil {
				t.Fatalf("Lookup(%q): %v", tc.id, err)
			}
			if got.ID != tc.wantID {
				t.Errorf("Lookup(%q).ID = %q, want %q", tc.id, got.ID, tc.wantID)
			}
		})
	}
}

// TestEveryDeclaredProfileIsLookupable is the completeness half: IDs() and
// the table behind Lookup cannot drift apart, so adding a profile without
// registering it fails here rather than at a customer's first start.
func TestEveryDeclaredProfileIsLookupable(t *testing.T) {
	t.Parallel()

	ids := profile.IDs()
	if len(ids) < 2 {
		t.Fatalf("IDs() = %v, want at least generic and ugos", ids)
	}
	for _, id := range ids {
		if _, err := profile.Lookup(string(id)); err != nil {
			t.Errorf("IDs() advertises %q but Lookup rejects it: %v", id, err)
		}
	}
}

// ---------------------------------------------------------------------
// A profile is semantically inert with respect to the backup domain
// ---------------------------------------------------------------------

// allowedProfileFields is the complete list of what EPIC B #81 and issue
// #167 permit a runtime profile to carry: an identity, platform capability
// reporting, a trusted native authentication gateway, a notification
// bridge, and the launch/presentation bridge (which UI bundle this profile
// serves). Anything else is the beginning of a fork.
var allowedProfileFields = map[string]string{
	"ID":           "the profile's own selector token",
	"PlatformID":   "the stable platform identifier the API reports",
	"DisplayName":  "presentation only",
	"Deployment":   "presentation only",
	"Capabilities": "platform capability reporting",
	"Gateway":      "the trusted native authentication gateway",
	"UIBundle":     "which UI bundle this profile's launch bridge serves",
}

// corePolicyMarkers are the backup-domain words a profile may never
// declare. They mirror scripts/architecture/ownership.go's rule set, which
// does not reach this package: layers.conf classifies apps/common/platform
// as core-layer, and that script only scans the platform and distribution
// layers. This test is what covers the gap for the one core-layer type
// whose whole job is to describe host-dependent behaviour.
var corePolicyMarkers = []string{"retention", "lifecycle", "validat", "catalog", "backupset", "backuppolicy", "schedule", "prune", "quarantine"}

// policyFields reports every field of typ that a runtime profile has no
// business carrying, checked two ways because the two catch different
// mistakes. The allow-list catches a field nobody has thought about, which
// is the common case; the marker scan catches a field somebody added to
// the allow-list without noticing it names something core owns, which is
// the case where the reviewer was the problem.
//
// It takes the type as a parameter rather than reading profile.Profile
// directly so the same code can be pointed at a deliberately forked struct
// and shown to fail. A checker only used against the thing it is meant to
// approve of has no way to demonstrate it can disapprove.
func policyFields(typ reflect.Type, allowed map[string]string) []string {
	var found []string
	for i := range typ.NumField() {
		f := typ.Field(i)
		if _, ok := allowed[f.Name]; !ok {
			found = append(found, f.Name+" (not in the allow-list)")
			continue
		}
		lower := strings.ToLower(f.Name)
		for _, marker := range corePolicyMarkers {
			if strings.Contains(lower, marker) {
				found = append(found, f.Name+" (names core-owned "+marker+")")
			}
		}
	}
	return found
}

func TestProfileCarriesNoBackupDomainPolicy(t *testing.T) {
	t.Parallel()

	if got := policyFields(reflect.TypeOf(profile.Profile{}), allowedProfileFields); len(got) != 0 {
		t.Errorf("profile.Profile declares %v; a runtime profile may only carry %v", got, sortedKeys(allowedProfileFields))
	}

	// Both directions. An allow-list entry naming a field that no longer
	// exists means the list has stopped describing the type, and a stale
	// allow-list is how this check quietly stops checking.
	typ := reflect.TypeOf(profile.Profile{})
	for name := range allowedProfileFields {
		if _, ok := typ.FieldByName(name); !ok {
			t.Errorf("the allow-list permits %q but profile.Profile has no such field, so the list is stale", name)
		}
	}
}

// TestPolicyFieldCheckerWouldNoticeAFork is the positive control for the
// test above. Without it, a checker that returned nil unconditionally
// would report a clean Profile forever.
func TestPolicyFieldCheckerWouldNoticeAFork(t *testing.T) {
	t.Parallel()

	type forkedProfile struct {
		ID              profile.ID
		Capabilities    capabilities.PlatformCapabilities
		RetentionTiers  int
		LifecycleStates []string
	}

	got := policyFields(reflect.TypeOf(forkedProfile{}), allowedProfileFields)
	if len(got) != 2 {
		t.Fatalf("the checker found %v on a deliberately forked profile, want both RetentionTiers and LifecycleStates", got)
	}
	joined := strings.Join(got, " ")
	for _, want := range []string{"RetentionTiers", "LifecycleStates"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the checker missed %s: %v", want, got)
		}
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ---------------------------------------------------------------------
// A profile never declares a capability it cannot deliver (§22)
// ---------------------------------------------------------------------

func TestNoProfileDeclaresACapabilityItCannotDeliver(t *testing.T) {
	t.Parallel()

	for _, id := range profile.IDs() {
		t.Run(string(id), func(t *testing.T) {
			t.Parallel()
			p := mustLookup(t, string(id))
			if p.Gateway != nil {
				p.Gateway.TrustedPeers = []string{"127.0.0.1/32"}
			}
			adapter, err := p.Adapter(profile.AdapterConfig{LocalAuth: stubAuthenticator{}})
			if err != nil {
				t.Fatalf("Adapter: %v", err)
			}
			if got := profile.UndeliverableCapabilities(adapter); len(got) != 0 {
				t.Errorf("profile %q declares %v but cannot deliver them", id, got)
			}
		})
	}
}

// TestUndeliverableCapabilityCheckWouldNoticeAnEmulatedClaim is the
// positive control: a profile that claims native notifications with no
// notifier behind it has to be caught, or the assertion above is vacuous.
func TestUndeliverableCapabilityCheckWouldNoticeAnEmulatedClaim(t *testing.T) {
	t.Parallel()

	got := profile.UndeliverableCapabilities(overclaimingAdapter{})
	if len(got) != 2 {
		t.Fatalf("UndeliverableCapabilities on an over-claiming adapter = %v, want both native auth and native notifications", got)
	}
	joined := strings.Join(got, " ")
	for _, want := range []string{"native_auth", "native_notifications"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the check missed %s: %v", want, got)
		}
	}
}

type overclaimingAdapter struct {
	capabilities.BasePlatformAdapter
}

func (overclaimingAdapter) ID() capabilities.PlatformID { return capabilities.PlatformUGOS }
func (overclaimingAdapter) Capabilities() capabilities.PlatformCapabilities {
	return capabilities.PlatformCapabilities{NativeAuth: true, NativeNotifications: true}
}
func (overclaimingAdapter) PlatformInfo(context.Context) (capabilities.PlatformInfo, error) {
	return capabilities.PlatformInfo{ID: capabilities.PlatformUGOS}, nil
}

type stubAuthenticator struct{}

func (stubAuthenticator) Authenticate(context.Context, capabilities.AuthRequest) (capabilities.AuthContext, error) {
	return capabilities.AuthContext{Authenticated: true, Username: "local", Mode: capabilities.AuthModeLocalAccount}, nil
}

func mustLookup(t *testing.T, id string) profile.Profile {
	t.Helper()
	p, err := profile.Lookup(id)
	if err != nil {
		t.Fatalf("Lookup(%q): %v", id, err)
	}
	return p
}

// ---------------------------------------------------------------------
// Fail-closed wiring
// ---------------------------------------------------------------------

func TestAdapterFailsClosed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		id      string
		mutate  func(*profile.Profile)
		cfg     profile.AdapterConfig
		wantErr error
	}{
		{
			name:    "a gateway profile with no trusted peer refuses to wire at all",
			id:      "ugos",
			cfg:     profile.AdapterConfig{LocalAuth: stubAuthenticator{}},
			wantErr: profile.ErrNoTrustedPeer,
		},
		{
			name:    "a local-auth profile with no authenticator refuses to wire",
			id:      "generic",
			cfg:     profile.AdapterConfig{},
			wantErr: profile.ErrNoAuthenticator,
		},
		{
			name:   "a gateway profile with a trusted peer wires",
			id:     "ugos",
			mutate: func(p *profile.Profile) { p.Gateway.TrustedPeers = []string{"10.0.0.0/8"} },
			cfg:    profile.AdapterConfig{LocalAuth: stubAuthenticator{}},
		},
		{
			name: "a local-auth profile with an authenticator wires",
			id:   "generic",
			cfg:  profile.AdapterConfig{LocalAuth: stubAuthenticator{}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := mustLookup(t, tc.id)
			if tc.mutate != nil {
				tc.mutate(&p)
			}
			_, err := p.Adapter(tc.cfg)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Adapter: unexpected error %v", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Adapter error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestGatewayRejectsAMalformedTrustedPeer(t *testing.T) {
	t.Parallel()

	p := mustLookup(t, "ugos")
	p.Gateway.TrustedPeers = []string{"not-a-cidr"}
	_, err := p.Adapter(profile.AdapterConfig{LocalAuth: stubAuthenticator{}})
	if err == nil {
		t.Fatal("a malformed trusted-peer range wired successfully; a gateway that cannot parse its own trust boundary must refuse")
	}
	if !strings.Contains(err.Error(), "not-a-cidr") {
		t.Errorf("the refusal %q does not name the value it could not parse", err)
	}
}

// ---------------------------------------------------------------------
// The trusted-gateway boundary
// ---------------------------------------------------------------------

func gatewayAuthenticator(t *testing.T, peers ...string) capabilities.Authenticator {
	t.Helper()
	p := mustLookup(t, "ugos")
	p.Gateway.TrustedPeers = peers
	adapter, err := p.Adapter(profile.AdapterConfig{LocalAuth: stubAuthenticator{}})
	if err != nil {
		t.Fatalf("Adapter: %v", err)
	}
	return adapter.Authenticator()
}

func TestTrustedGatewayIdentity(t *testing.T) {
	t.Parallel()

	const spoofed = "attacker-as-admin"

	cases := []struct {
		name       string
		remoteAddr string
		headers    map[string]string
		wantUser   string
		wantErr    error
	}{
		{
			name:       "the positive control: a genuinely gateway-authenticated request succeeds",
			remoteAddr: "10.4.0.9:41000",
			headers:    map[string]string{"X-Ugos-User": "operator"},
			wantUser:   "operator",
		},
		{
			name:       "a direct-LAN request carrying forged identity headers is refused",
			remoteAddr: "192.168.1.50:52000",
			headers:    map[string]string{"X-Ugos-User": spoofed},
			wantErr:    profile.ErrUntrustedPeer,
		},
		{
			name:       "a trusted peer that sends no identity at all is refused, distinguishably",
			remoteAddr: "10.4.0.9:41000",
			headers:    map[string]string{},
			wantErr:    profile.ErrNoGatewayIdentity,
		},
		{
			name:       "a trusted peer sending an empty identity is refused, not treated as anonymous-but-allowed",
			remoteAddr: "10.4.0.9:41000",
			headers:    map[string]string{"X-Ugos-User": "   "},
			wantErr:    profile.ErrNoGatewayIdentity,
		},
		{
			name:       "an unparsable remote address is untrusted, never trusted by accident",
			remoteAddr: "garbage",
			headers:    map[string]string{"X-Ugos-User": spoofed},
			wantErr:    profile.ErrUntrustedPeer,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			auth := gatewayAuthenticator(t, "10.4.0.0/16")
			h := http.Header{}
			for k, v := range tc.headers {
				h.Set(k, v)
			}

			got, err := auth.Authenticate(context.Background(), capabilities.AuthRequest{
				Headers:    h,
				RemoteAddr: tc.remoteAddr,
			})

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Authenticate error = %v, want %v", err, tc.wantErr)
				}
				if got.Authenticated {
					t.Error("a refused request came back Authenticated")
				}
				if strings.Contains(got.Username, spoofed) {
					t.Errorf("the refused AuthContext carries the spoofed username %q", got.Username)
				}
				return
			}

			if err != nil {
				t.Fatalf("Authenticate: %v", err)
			}
			if !got.Authenticated || got.Username != tc.wantUser {
				t.Errorf("Authenticate = %+v, want an authenticated %q", got, tc.wantUser)
			}
			if got.Mode != capabilities.AuthModeNativeSession {
				t.Errorf("Mode = %q, want %q", got.Mode, capabilities.AuthModeNativeSession)
			}
		})
	}
}

// TestSanitizeStripsIdentityFromAnUntrustedPeer proves the "stripped or
// ignored" half literally: a handler downstream of the gateway boundary
// must not even be able to read the forged header.
func TestSanitizeStripsIdentityFromAnUntrustedPeer(t *testing.T) {
	t.Parallel()

	p := mustLookup(t, "ugos")
	p.Gateway.TrustedPeers = []string{"10.4.0.0/16"}
	g, err := p.Gateway.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	untrusted := http.Header{"X-Ugos-User": []string{"attacker"}, "Accept": []string{"application/json"}}
	g.Sanitize(untrusted, "192.168.1.50:52000")
	if v := untrusted.Get("X-Ugos-User"); v != "" {
		t.Errorf("the identity header survived an untrusted peer: %q", v)
	}
	if untrusted.Get("Accept") != "application/json" {
		t.Error("Sanitize removed a header that is none of its business")
	}

	// Positive control: from the trusted peer the same header is left
	// alone, so the assertion above is about trust and not about the
	// function deleting everything it sees.
	trusted := http.Header{"X-Ugos-User": []string{"operator"}}
	g.Sanitize(trusted, "10.4.0.9:41000")
	if v := trusted.Get("X-Ugos-User"); v != "operator" {
		t.Errorf("Sanitize stripped the identity header from the TRUSTED peer too (got %q), so it is deleting rather than deciding", v)
	}
}

// TestAdapterRefusesAProfileThatDeclaresAnUndeliverableCapability is the
// wiring half of the §22 refusal above. Until Adapter ran the check, the
// only two callers of UndeliverableCapabilities were tests, so a profile
// row could declare a capability nothing behind it delivers and the
// running process would report it to the UI as supported. Two of the five
// capabilities were also never probed at all, which is why the three
// browser-host ones are exercised here rather than only the two with a
// collaborator.
func TestAdapterRefusesAProfileThatDeclaresAnUndeliverableCapability(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		caps capabilities.PlatformCapabilities
		want string
	}{
		{"a storage picker, which no server-side adapter can offer", capabilities.PlatformCapabilities{StoragePicker: true}, "storage_picker"},
		{"an embedded window", capabilities.PlatformCapabilities{EmbeddedWindow: true}, "embedded_window"},
		{"app-store packaging", capabilities.PlatformCapabilities{AppStorePackaging: true}, "app_store_packaging"},
		{"native notifications with no notifier behind them", capabilities.PlatformCapabilities{NativeNotifications: true}, "native_notifications"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := profile.Profile{
				ID:           "synthetic",
				PlatformID:   capabilities.PlatformGeneric,
				DisplayName:  "a synthetic profile that declares more than its wiring delivers",
				Capabilities: tc.caps,
			}
			_, err := p.Adapter(profile.AdapterConfig{LocalAuth: stubAuthenticator{}})
			if !errors.Is(err, profile.ErrUndeliverableCapability) {
				t.Fatalf("Adapter error = %v, want %v: a declared capability nothing delivers has to refuse at wiring time, not be reported to the UI as supported", err, profile.ErrUndeliverableCapability)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Adapter error = %q, which never names the undeliverable capability %q", err, tc.want)
			}
		})
	}
}

// TestAdapterWiresWhatItCanActuallyDeliver is the control for the refusal
// above, and it is the half that matters: without it, an Adapter that
// refused every profile would pass every assertion in that test. The same
// synthetic profile wires once the declaration matches the wiring, both
// by dropping the claim and by supplying the collaborator it promised.
func TestAdapterWiresWhatItCanActuallyDeliver(t *testing.T) {
	t.Parallel()

	base := profile.Profile{
		ID:          "synthetic",
		PlatformID:  capabilities.PlatformGeneric,
		DisplayName: "a synthetic profile",
	}

	t.Run("declaring nothing", func(t *testing.T) {
		t.Parallel()
		if _, err := base.Adapter(profile.AdapterConfig{LocalAuth: stubAuthenticator{}}); err != nil {
			t.Fatalf("Adapter: %v, want a working adapter for a profile that declares no capability at all", err)
		}
	})

	t.Run("declaring native notifications AND supplying the notifier", func(t *testing.T) {
		t.Parallel()
		p := base
		p.Capabilities = capabilities.PlatformCapabilities{NativeNotifications: true}
		_, err := p.Adapter(profile.AdapterConfig{LocalAuth: stubAuthenticator{}, Notifier: stubNotifier{}})
		if err != nil {
			t.Fatalf("Adapter: %v, want the declaration to be accepted once a real notifier is behind it: the check is behavioural, not a ban on declaring anything", err)
		}
	})
}

type stubNotifier struct{}

func (stubNotifier) Notify(context.Context, string, string) error { return nil }
