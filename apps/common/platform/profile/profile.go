// Package profile is the runtime-profile selector (issue #167, which
// absorbed #168): the one mechanism by which the single canonical
// executable changes host-dependent behaviour without becoming a second
// build.
//
// # What a profile is, and what it deliberately is not
//
// A runtime profile may change four things, and EPIC B #81 names them:
// a trusted native authentication gateway, a provider notification
// bridge, a platform launch or navigation bridge, and platform capability
// reporting. It may not change backup lifecycle, retention or validation
// semantics, and it may not change authorization semantics either. A
// profile can supply an identity, it cannot decide what that identity may
// do. That distinction is what makes a platform integration an adapter
// rather than a fork, and profile_test.go asserts it structurally
// (Profile may declare nothing outside an allow-list) as well as
// behaviourally (the parity suite in apps/common/webhost/serve).
//
// # Why the profile table lives here rather than under apps/<platform>
//
// scripts/architecture/layers.conf classifies apps/common/platform as
// core-layer, and every apps/<platform> directory as runtime-platform.
// The profile TABLE is core-layer on purpose: runtime selection means one
// binary carries every profile, so a table split across per-platform
// packages would reintroduce exactly the per-platform build section 3.7
// forbids. What stays at the platform layer is the host-specific
// behaviour a profile points at.
//
// One consequence is worth stating out loud: scripts/architecture's
// ownership check only scans the platform and distribution layers, so it
// does not reach this package. TestProfileCarriesNoBackupDomainPolicy is
// what covers that gap, and it is stricter than the scanner: an
// allow-list rather than a marker list.
package profile

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strings"

	"github.com/spdrman/rclone-manager/apps/common/platform/capabilities"
)

// ID is a runtime profile's selector token, exactly as it appears after
// `--profile=` on the command line and in the canonical Compose runtime
// definition's own `x-canonical-runtime.profiles` list.
type ID string

const (
	// Generic is the profile every deployment gets unless it asks for
	// another one: no native platform integration at all, local
	// authentication, and the shared UI's generic bridge.
	Generic ID = "generic"

	// UGOS is the UGREEN UGOS Pro profile. It exists here, in the one
	// canonical executable, rather than in a UGOS build: everything this
	// issue has to prove about it is provable against a synthetic
	// gateway, and no part of it needs a UGREEN device.
	UGOS ID = "ugos"

	// The five platforms Phase 4 shipped, converted to thin adapters
	// over this runtime by issue #169.
	//
	// None of them declares a capability or a gateway, and that is the
	// finding rather than an omission. Every one of them uses §13A local
	// authentication, none has a server-side notification channel, and
	// the three remaining capabilities their frontend bridges declare
	// (the embedded window, the storage picker, app-store packaging) are
	// browser-host capabilities this Go process cannot deliver; §22
	// forbids declaring a capability that would have to be emulated. So
	// what a profile changes for these five is exactly what legitimately
	// differs: which platform the runtime reports itself as, how the
	// deployment is described, and which UI bridge the Web UI host
	// serves.
	//
	// That is not nothing. Before this, a user who installed through the
	// TrueNAS catalog or Community Applications was told by the running
	// application that this was a generic Docker Compose deployment,
	// because the platform identity was a build-time constant and the
	// build never set it.
	TrueNAS        ID = "truenas"
	Unraid         ID = "unraid"
	OpenMediaVault ID = "openmediavault"
	Proxmox        ID = "proxmox"
	Synology       ID = "synology"
)

// ErrUnknownProfile is returned by Lookup for a selector nothing
// implements. It is deliberately not a fallback to Generic: a deployment
// that asked for ugos and silently got the generic auth story is the
// failure this refusal exists to prevent.
var ErrUnknownProfile = errors.New("unknown runtime profile")

// Profile is the complete description of one runtime profile.
//
// Every field here is one of the four things EPIC B #81 permits a profile
// to change, plus its own identity. Adding a field is a deliberate act
// that fails TestProfileCarriesNoBackupDomainPolicy until the allow-list
// next to that test is extended with a reason.
type Profile struct {
	// ID is this profile's selector token.
	ID ID

	// PlatformID is the stable platform identifier GET
	// /api/v1/system/capabilities reports. It is a separate field from ID
	// because the profile selector and the wire-level platform identity
	// are two contracts that happen to agree today, and collapsing them
	// would make disagreeing later a breaking change rather than an edit.
	PlatformID capabilities.PlatformID

	// DisplayName and Deployment are presentation only.
	DisplayName string
	Deployment  string

	// Capabilities is what this profile reports the host platform can do.
	// A profile may only declare a capability the running process can
	// actually deliver; UndeliverableCapabilities is the check, and
	// Adapter runs it on every wiring rather than leaving it to a test.
	Capabilities capabilities.PlatformCapabilities

	// Gateway, when non-nil, is this profile's trusted native
	// authentication gateway. nil means the profile authenticates through
	// the local-account service instead.
	Gateway *Gateway

	// UIBundle names the UI bundle directory this profile's launch bridge
	// serves, under the bundle root the web host was given. Empty means
	// "the bundle compiled into the binary".
	UIBundle string
}

// registry is the profile table. It is a function rather than a package
// variable so a caller always gets its own Gateway pointer to configure:
// a shared *Gateway would let one deployment's trusted-peer list leak
// into another's, which in a test harness is a wrong result and in a
// long-lived process would be a security bug.
func registry() map[ID]Profile {
	return map[ID]Profile{
		Generic: {
			ID:          Generic,
			PlatformID:  capabilities.PlatformGeneric,
			DisplayName: "Generic Docker / Linux",
			Deployment:  "Docker Compose",
			// Every capability false, never emulated (§22). Local
			// authentication is this profile's own fallback, not a
			// platform-provided session, so it is deliberately NOT
			// NativeAuth.
			Capabilities: capabilities.PlatformCapabilities{},
			UIBundle:     "generic",
		},
		UGOS: {
			ID:          UGOS,
			PlatformID:  capabilities.PlatformUGOS,
			DisplayName: "UGREEN UGOS Pro",
			Deployment:  "UGOS package",
			// NativeAuth only, and that is not an oversight. The other
			// four capabilities the UGOS bridge declares (native
			// notifications, the storage picker, the embedded window, app
			// store packaging) are browser-host capabilities this Go
			// process cannot deliver, and §22 forbids declaring a
			// capability that is emulated rather than provided. The
			// bridge in apps/ugos/frontend remains the authority for its
			// own half; this is the authority for the server's.
			Capabilities: capabilities.PlatformCapabilities{NativeAuth: true},
			Gateway: &Gateway{
				UsernameHeader: "X-Ugos-User",
			},
			UIBundle: "ugos",
		},

		// The five converted Phase 4 platforms. Each one is a row and
		// nothing else: no gateway, no capability, no code path. A
		// platform that needed more than a row would not be a thin
		// adapter, which is the whole test issue #169 applies.
		TrueNAS:        containerProfile(TrueNAS, capabilities.PlatformTrueNAS, "TrueNAS", "TrueNAS Apps"),
		Unraid:         containerProfile(Unraid, capabilities.PlatformUnraid, "Unraid", "Unraid Docker template"),
		OpenMediaVault: containerProfile(OpenMediaVault, capabilities.PlatformOpenMediaVault, "OpenMediaVault", "OpenMediaVault Compose deployment"),
		Proxmox:        containerProfile(Proxmox, capabilities.PlatformProxmox, "Proxmox VE", "Proxmox VE container-host guest"),
		Synology:       containerProfile(Synology, capabilities.PlatformSynology, "Synology DSM", "Synology Container Manager project"),
	}
}

// containerProfile is one converted platform's row: an identity, a
// description and its own UI bundle, over the same runtime as every other
// one.
//
// A constructor rather than five literals, because five near-identical
// literals is precisely how one of them silently acquires a capability
// nobody meant to grant. Anything a platform genuinely needs beyond an
// identity is a deliberate edit that stops using this.
func containerProfile(id ID, platform capabilities.PlatformID, displayName, deployment string) Profile {
	return Profile{
		ID:          id,
		PlatformID:  platform,
		DisplayName: displayName,
		Deployment:  deployment,
		// Every capability false, never emulated (§22). Local
		// authentication is this profile's own fallback, not a
		// platform-provided session, so it is deliberately NOT
		// NativeAuth.
		Capabilities: capabilities.PlatformCapabilities{},
		UIBundle:     string(id),
	}
}

// Lookup resolves a selector token. An unrecognised or empty token is
// refused, and the refusal names what is valid.
func Lookup(id string) (Profile, error) {
	table := registry()
	p, ok := table[ID(id)]
	if !ok {
		known := make([]string, 0, len(table))
		for k := range table {
			known = append(known, string(k))
		}
		sort.Strings(known)
		return Profile{}, fmt.Errorf("%w %q (known profiles: %s)", ErrUnknownProfile, id, strings.Join(known, ", "))
	}
	return p, nil
}

// Profile resolves this ID to its row of the table. It panics for an ID
// that is not a declared constant of this package, which cannot happen
// from outside it: every other caller goes through Lookup, which returns
// an error. This exists so a caller that already has a compile-time
// constant does not have to handle an impossible error.
func (id ID) Profile() Profile {
	p, err := Lookup(string(id))
	if err != nil {
		panic("profile: " + err.Error())
	}
	return p
}

// IdentityHeaders lists every provider-native identity header ANY profile
// in the table declares, sorted and deduplicated.
//
// A hop that has to remove an untrusted identity needs this list rather
// than one profile's own header. The browser-facing edge does not know
// which profile the engine behind it was started with, and a deployment
// running the generic profile today can be restarted onto a gateway
// profile tomorrow without the header ever having been scrubbed in
// between. Stripping the whole set costs one map lookup per header and
// removes the class rather than one instance of it.
func IdentityHeaders() []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range registry() {
		if p.Gateway == nil {
			continue
		}
		h := strings.TrimSpace(p.Gateway.UsernameHeader)
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// StripIdentityHeaders removes every header IdentityHeaders names from h.
// Unconditional: a caller that has decided the peer is not trusted has
// nothing left to decide.
func StripIdentityHeaders(h http.Header) {
	for _, name := range IdentityHeaders() {
		h.Del(name)
	}
}

// IDs lists every implemented profile, sorted.
func IDs() []ID {
	table := registry()
	out := make([]ID, 0, len(table))
	for k := range table {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ---------------------------------------------------------------------
// Wiring a profile into a PlatformAdapter
// ---------------------------------------------------------------------

var (
	// ErrNoAuthenticator is returned when a local-account profile is
	// wired with no local authentication service behind it. Serving an
	// unauthenticated destructive API to the LAN is the outcome this
	// refusal exists to prevent, so it is a refusal and not a warning.
	ErrNoAuthenticator = errors.New("this profile authenticates locally but no local authenticator was supplied")

	// ErrUndeliverableCapability is returned when a profile declares a
	// capability the adapter this wiring produced cannot actually
	// deliver. It is a refusal and not a warning for the same reason the
	// two above are: a capability reported to the UI as supported is
	// acted on, and §22 says an unsupported capability is explicit,
	// never emulated.
	ErrUndeliverableCapability = errors.New("this profile declares a capability its wiring cannot deliver")

	// ErrNoTrustedPeer is returned when a gateway profile is wired with
	// no trusted peer range. Without one there is no gateway, only an
	// identity header anyone on the LAN can set.
	ErrNoTrustedPeer = errors.New("this profile trusts a platform gateway but no trusted peer range was configured")
)

// AdapterConfig is what Adapter needs from the process wiring it up.
type AdapterConfig struct {
	// LocalAuth is the local-account authenticator, used by a profile
	// with no gateway of its own.
	LocalAuth capabilities.Authenticator

	// Notifier, when non-nil, is the platform notification bridge. A
	// profile that declares NativeNotifications needs one; today no
	// profile does, and supplying one without the declaration changes
	// nothing.
	Notifier capabilities.Notifier
}

// Adapter builds this profile's capabilities.PlatformAdapter, or refuses.
// Every refusal is fail-closed: there is no branch here that produces a
// working adapter out of an incomplete configuration.
func (p Profile) Adapter(cfg AdapterConfig) (capabilities.PlatformAdapter, error) {
	a := &adapter{profile: p, notifier: cfg.Notifier}

	if p.Gateway != nil {
		g, err := p.Gateway.Compile()
		if err != nil {
			return nil, err
		}
		a.authenticator = g
	} else {
		if cfg.LocalAuth == nil {
			return nil, fmt.Errorf("profile %q: %w", p.ID, ErrNoAuthenticator)
		}
		a.authenticator = cfg.LocalAuth
	}

	// §22's refusal, run rather than described. Until this call the check
	// existed and had tests, and no process ever invoked it, so a profile
	// row declaring a capability its wiring cannot deliver would have
	// reached a running deployment and been reported to the UI as
	// supported. Wiring is the only place that can answer the question,
	// because the answer depends on what this particular call supplied.
	if undeliverable := UndeliverableCapabilities(a); len(undeliverable) != 0 {
		return nil, fmt.Errorf("profile %q: %w: %s", p.ID, ErrUndeliverableCapability, strings.Join(undeliverable, "; "))
	}
	return a, nil
}

type adapter struct {
	capabilities.BasePlatformAdapter
	profile       Profile
	authenticator capabilities.Authenticator
	notifier      capabilities.Notifier
}

func (a *adapter) ID() capabilities.PlatformID { return a.profile.PlatformID }

func (a *adapter) Capabilities() capabilities.PlatformCapabilities { return a.profile.Capabilities }

func (a *adapter) Authenticator() capabilities.Authenticator { return a.authenticator }

func (a *adapter) Notifier() capabilities.Notifier {
	if a.notifier != nil {
		return a.notifier
	}
	return a.BasePlatformAdapter.Notifier()
}

func (a *adapter) PlatformInfo(context.Context) (capabilities.PlatformInfo, error) {
	return capabilities.PlatformInfo{
		ID:         a.profile.PlatformID,
		Name:       a.profile.DisplayName,
		Deployment: a.profile.Deployment,
	}, nil
}

// UndeliverableCapabilities reports every capability an adapter declares
// that it cannot actually deliver (§22: an unsupported capability is
// explicit, never emulated). The check is behavioural rather than
// declarative: it asks the adapter for the collaborator each capability
// promises and sees whether the answer is the null object.
func UndeliverableCapabilities(a capabilities.PlatformAdapter) []string {
	var out []string
	caps := a.Capabilities()

	if caps.NativeAuth {
		_, err := a.Authenticator().Authenticate(context.Background(), capabilities.AuthRequest{Headers: http.Header{}})
		if errors.Is(err, capabilities.ErrCapabilityUnsupported) {
			out = append(out, "native_auth: declared, but Authenticator() is the unsupported null object")
		}
	}
	if caps.NativeNotifications {
		if err := a.Notifier().Notify(context.Background(), "probe", "probe"); errors.Is(err, capabilities.ErrCapabilityUnsupported) {
			out = append(out, "native_notifications: declared, but Notifier() is the unsupported null object")
		}
	}

	// The remaining three capabilities have no collaborator to probe,
	// and that IS the finding: capabilities.PlatformAdapter offers
	// exactly two, Authenticator() and Notifier(), so a server-side
	// adapter has nothing it could deliver a storage picker, an embedded
	// window or app-store packaging THROUGH. Those three are facts about
	// the browser host a provider bridge runs inside, reported to the UI
	// over GET /api/v1/system/capabilities, and this side of the wire
	// declaring one would be exactly the emulated claim §22 forbids.
	// Without these three the check probed two of five, which is how a
	// future profile row could declare one and be caught by nothing.
	for _, c := range []struct {
		declared bool
		name     string
	}{
		{caps.StoragePicker, "storage_picker"},
		{caps.EmbeddedWindow, "embedded_window"},
		{caps.AppStorePackaging, "app_store_packaging"},
	} {
		if c.declared {
			out = append(out, c.name+": declared server-side, but a capabilities.PlatformAdapter has no collaborator that could deliver it (it is a browser-host fact, not a process one)")
		}
	}
	return out
}

// ---------------------------------------------------------------------
// The trusted-gateway boundary
// ---------------------------------------------------------------------

var (
	// ErrUntrustedPeer means the request did not arrive from a configured
	// trusted gateway, so any identity header it carried was ignored.
	ErrUntrustedPeer = errors.New("request did not arrive from a trusted platform gateway")

	// ErrNoGatewayIdentity means the request DID arrive from the trusted
	// gateway but carried no usable identity. Distinguishable from
	// ErrUntrustedPeer on purpose: one is an attack and the other is a
	// misconfigured gateway, and an operator has to be able to tell.
	ErrNoGatewayIdentity = errors.New("the trusted platform gateway supplied no identity")

	// ErrAmbiguousIdentity means the identity header arrived more than
	// once, so the request names more than one caller.
	//
	// A gateway that REPLACES its header (nginx's proxy_set_header) sends
	// exactly one value; a gateway that appends one, or any proxy that
	// passes the client's copy through alongside its own, sends two. Go's
	// Header.Get returns the FIRST, which on the wire is the client's, so
	// resolving this silently hands the identity to whoever sent the
	// request rather than to whoever the gateway authenticated. There is
	// no safe value to pick, so this refuses instead of picking one.
	ErrAmbiguousIdentity = errors.New("the identity header arrived more than once, so the request names more than one caller")
)

// Gateway is a profile's trusted native authentication gateway: the
// declaration that identity arriving in a header may be believed, but
// only from a verified network source.
//
// #92 (C1.2) proves the same boundary against the real UGOS gateway on
// real hardware. This is the generic and profile-selection side of it,
// and the two must not diverge: the rule both enforce is that a
// provider-native identity header is accepted on the strength of the peer
// it arrived from, never on the strength of the header being present.
type Gateway struct {
	// TrustedPeers are CIDR ranges, in text form so a profile table and a
	// command-line flag can carry the same representation. Empty means no
	// gateway is configured, which is a refusal rather than a default.
	TrustedPeers []string

	// UsernameHeader is the header the platform gateway sets.
	UsernameHeader string
}

// CompiledGateway is a Gateway with its trust boundary parsed. Producing
// one is the only way to get an Authenticator out of a Gateway, so a
// malformed range can never reach a request.
type CompiledGateway struct {
	peers          []netip.Prefix
	usernameHeader string
}

// Compile parses the trust boundary, or refuses.
func (g *Gateway) Compile() (*CompiledGateway, error) {
	if g == nil || len(g.TrustedPeers) == 0 {
		return nil, ErrNoTrustedPeer
	}
	if strings.TrimSpace(g.UsernameHeader) == "" {
		return nil, errors.New("gateway: no identity header is configured, so there is nothing to read an identity from")
	}
	out := &CompiledGateway{usernameHeader: g.UsernameHeader}
	for _, raw := range g.TrustedPeers {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("gateway: %q is not a CIDR range: %w", raw, err)
		}
		out.peers = append(out.peers, prefix)
	}
	return out, nil
}

// UsernameHeader is the identity header this gateway is entitled to set.
func (c *CompiledGateway) UsernameHeader() string { return c.usernameHeader }

// Trusts reports whether remoteAddr is one of the configured peers. An
// address that cannot be parsed is untrusted: the only safe reading of
// "I do not know where this came from".
func (c *CompiledGateway) Trusts(remoteAddr string) bool {
	addr, ok := parseAddr(remoteAddr)
	if !ok {
		return false
	}
	for _, prefix := range c.peers {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func parseAddr(remoteAddr string) (netip.Addr, bool) {
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	// A netip.Addr carrying a zone ("fe80::1%en0") compares unequal to the
	// same address without one, so the zone is dropped before the prefix
	// test rather than after it.
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap().WithZone(""), true
}

// Sanitize removes provider-native identity headers a request is not
// entitled to carry, so nothing downstream can read one even by accident.
// It leaves every other header alone.
//
// From an untrusted peer, every identity header any profile declares is
// removed, this gateway's included. From the trusted peer, this gateway's
// own header survives and every OTHER profile's is still removed: the
// gateway authenticated one identity, not a set, and a header belonging
// to a profile this process is not running has no provenance at all.
func (c *CompiledGateway) Sanitize(h http.Header, remoteAddr string) {
	trusted := c.Trusts(remoteAddr)
	for _, name := range IdentityHeaders() {
		if trusted && http.CanonicalHeaderKey(name) == http.CanonicalHeaderKey(c.usernameHeader) {
			continue
		}
		h.Del(name)
	}
	if !trusted {
		h.Del(c.usernameHeader)
	}
}

// Authenticate resolves the caller's identity, or refuses. The refusal
// branches never populate the returned AuthContext, so a caller that
// ignores the error still cannot read a spoofed username out of it.
func (c *CompiledGateway) Authenticate(_ context.Context, r capabilities.AuthRequest) (capabilities.AuthContext, error) {
	if !c.Trusts(r.RemoteAddr) {
		return capabilities.AuthContext{}, fmt.Errorf("%w (peer %q)", ErrUntrustedPeer, r.RemoteAddr)
	}
	username := ""
	if r.Headers != nil {
		// Values, not Get: Get would silently answer with the first of
		// several, which is the smuggled one. See ErrAmbiguousIdentity.
		values := r.Headers.Values(c.usernameHeader)
		if len(values) > 1 {
			return capabilities.AuthContext{}, fmt.Errorf("%w (header %s, %d values)", ErrAmbiguousIdentity, c.usernameHeader, len(values))
		}
		if len(values) == 1 {
			username = strings.TrimSpace(values[0])
		}
	}
	if username == "" {
		return capabilities.AuthContext{}, fmt.Errorf("%w (header %s)", ErrNoGatewayIdentity, c.usernameHeader)
	}
	return capabilities.AuthContext{
		Authenticated: true,
		Username:      username,
		Mode:          capabilities.AuthModeNativeSession,
	}, nil
}
