// Source identity, tested for the one property that matters: a source that
// MOVED is still the same source.
//
// An incremental engine's whole value comes from finding the previous
// snapshot of the same source and reusing its content. What it uses to find
// it is an identity, and if that identity contains anything about how this
// deployment happened to reach the source -- a container bind mount, an
// install prefix, a staging directory -- then a perfectly ordinary
// operational change forks the lineage: the next run finds no predecessor,
// re-reads every byte, stores a second full copy, and every retention and
// last-known-good decision downstream is now being made about two
// unrelated-looking streams that are the same data. Nothing fails. The
// deployment just silently doubles and loses its history.
//
// So the tests below move the source around in every way a real deployment
// moves it and require the identity to hold, and then require it to change
// when the source genuinely is a different one.

package model

import (
	"strings"
	"testing"
)

// setUUID is one backup set's durable identifier, fixed here so every case
// below is varying exactly one thing.
const setUUID = "6f1d2b7a-1c4e-4f8b-9a2d-3e5c7b9d1f00"

// TestSourceIdentity_SurvivesAChangedMountPoint is the container case, and
// it is not hypothetical: the same source directory is reached at
// /mnt/user/appdata on Unraid, /mnt/tank/apps on TrueNAS, and /volume1/docker
// on DSM, and a deployment that moves a disk or renames a share changes it
// again.
func TestSourceIdentity_SurvivesAChangedMountPoint(t *testing.T) {
	t.Parallel()

	before := mustIdentity(t, SourceIdentityInput{
		SetUUID:  setUUID,
		Endpoint: SourceEndpoint{Kind: EndpointLocal},
		Root:     SourceRoot{Path: "/mnt/user/appdata/postgres/data", MountPrefix: "/mnt/user/appdata"},
	})

	after := mustIdentity(t, SourceIdentityInput{
		SetUUID:  setUUID,
		Endpoint: SourceEndpoint{Kind: EndpointLocal},
		Root:     SourceRoot{Path: "/mnt/tank/apps/postgres/data", MountPrefix: "/mnt/tank/apps"},
	})

	if before != after {
		t.Errorf("moving the mount point forked the source identity:\n  before %s\n  after  %s\n"+
			"every later run would find no predecessor snapshot, re-read the whole source and store a second full copy", before, after)
	}
}

// TestSourceIdentity_SurvivesAChangedInstallPrefix is the same property for
// the other path a deployment can relocate: the product's own install
// prefix, which is what changes when a package moves from /opt to
// /usr/local, or when a test harness runs the whole thing under a
// temporary directory.
func TestSourceIdentity_SurvivesAChangedInstallPrefix(t *testing.T) {
	t.Parallel()

	identities := map[string]SourceIdentity{}
	for _, prefix := range []string{
		"/opt/backupd",
		"/usr/local/backupd",
		"/var/folders/9k/T/TestRun2847163094/001", // what t.TempDir() hands out
	} {
		identities[prefix] = mustIdentity(t, SourceIdentityInput{
			SetUUID:  setUUID,
			Endpoint: SourceEndpoint{Kind: EndpointLocal},
			Root:     SourceRoot{Path: prefix + "/sources/uploads", MountPrefix: prefix},
		})
	}

	var first SourceIdentity
	for prefix, id := range identities {
		if first == "" {
			first = id
			continue
		}
		if id != first {
			t.Errorf("the install prefix %q produced a different identity (%s) from another prefix over the same source (%s)", prefix, id, first)
		}
	}
}

// TestSourceIdentity_IgnoresTheTrailingShapeOfAPath is the cosmetic half of
// the same problem. "/srv/data/", "/srv/data" and "/srv/./data" are one
// directory written three ways, and a config edit that adds a trailing
// slash must not fork a lineage.
func TestSourceIdentity_IgnoresTheTrailingShapeOfAPath(t *testing.T) {
	t.Parallel()

	want := mustIdentity(t, SourceIdentityInput{
		SetUUID:  setUUID,
		Endpoint: SourceEndpoint{Kind: EndpointSFTP, Host: "production.example.internal", User: "backup"},
		Root:     SourceRoot{Path: "/backups/postgres"},
	})

	for _, spelling := range []string{"/backups/postgres/", "/backups/./postgres", "//backups//postgres"} {
		got := mustIdentity(t, SourceIdentityInput{
			SetUUID:  setUUID,
			Endpoint: SourceEndpoint{Kind: EndpointSFTP, Host: "production.example.internal", User: "backup"},
			Root:     SourceRoot{Path: spelling},
		})
		if got != want {
			t.Errorf("path %q produced %s, want %s; three spellings of one directory must be one lineage", spelling, got, want)
		}
	}
}

// TestSourceIdentity_IgnoresTheEndpointsSpellingButNotItsIdentity is the
// endpoint half: a host is case-insensitive and an omitted port is the
// default port, so neither may fork a lineage -- while a different host, a
// different user or a genuinely different port must.
func TestSourceIdentity_IgnoresTheEndpointsSpellingButNotItsIdentity(t *testing.T) {
	t.Parallel()

	base := SourceIdentityInput{
		SetUUID:  setUUID,
		Endpoint: SourceEndpoint{Kind: EndpointSFTP, Host: "production.example.internal", User: "backup"},
		Root:     SourceRoot{Path: "/backups/postgres"},
	}
	want := mustIdentity(t, base)

	// The same endpoint, spelled differently.
	for _, same := range []SourceEndpoint{
		{Kind: EndpointSFTP, Host: "Production.Example.Internal", User: "backup"},           // DNS is case-insensitive
		{Kind: EndpointSFTP, Host: "production.example.internal", User: "backup", Port: 22}, // the default, written out
	} {
		in := base
		in.Endpoint = same
		if got := mustIdentity(t, in); got != want {
			t.Errorf("endpoint %+v produced %s, want %s; the same endpoint spelled differently is the same endpoint", same, got, want)
		}
	}

	// A different endpoint.
	for _, other := range []SourceEndpoint{
		{Kind: EndpointSFTP, Host: "staging.example.internal", User: "backup"},
		{Kind: EndpointSFTP, Host: "production.example.internal", User: "restore"},
		{Kind: EndpointSFTP, Host: "production.example.internal", User: "backup", Port: 2222},
		{Kind: EndpointLocal},
	} {
		in := base
		in.Endpoint = other
		if got := mustIdentity(t, in); got == want {
			t.Errorf("endpoint %+v produced the same identity as %+v; two different sources sharing a lineage means one snapshot stream contains both", other, base.Endpoint)
		}
	}
}

// TestSourceIdentity_EndpointSubFieldsCannotBleedIntoEachOther is the
// review finding from #826: an endpoint whose fields are concatenated into
// one string before hashing lets a separator inside a field impersonate the
// separator between two fields.
//
// The two endpoints below are genuinely different -- one is user "a@b" on
// host "c", the other is user "a" on host "b@c" -- and an sftp user
// containing an @ is ordinary rather than exotic (it is how a domain or
// tenant is written on a good deal of managed sftp). Concatenated as
// "sftp://user@host:port" both render "sftp://a@b@c:22", so they hash
// identically: two different sources on one snapshot lineage, where each
// run looks like the other's tree having changed completely and one set's
// retention prunes the other's restore points.
//
// The fix is that the endpoint's kind, user, host and port are four
// separate labelled, length-prefixed fields in the canonical form, exactly
// as the set identifier and the root already are, so no value a field can
// hold can reach across the field boundary.
func TestSourceIdentity_EndpointSubFieldsCannotBleedIntoEachOther(t *testing.T) {
	t.Parallel()

	base := SourceIdentityInput{
		SetUUID: setUUID,
		Root:    SourceRoot{Path: "/backups/postgres"},
	}

	userCarriesTheAt := base
	userCarriesTheAt.Endpoint = SourceEndpoint{Kind: EndpointSFTP, User: "a@b", Host: "c"}

	hostCarriesTheAt := base
	hostCarriesTheAt.Endpoint = SourceEndpoint{Kind: EndpointSFTP, User: "a", Host: "b@c"}

	if got, other := mustIdentity(t, userCarriesTheAt), mustIdentity(t, hostCarriesTheAt); got == other {
		t.Errorf("user=%q host=%q and user=%q host=%q both produced %s; the endpoint's fields are being concatenated before hashing, so a value containing the separator impersonates the field boundary and two different sources share one lineage",
			userCarriesTheAt.Endpoint.User, userCarriesTheAt.Endpoint.Host,
			hostCarriesTheAt.Endpoint.User, hostCarriesTheAt.Endpoint.Host, got)
	}

	// The same hazard on the other side of the port separator: a host
	// ending in ":2222" and the default port must not be the same bytes as
	// that host on port 2222.
	hostCarriesThePort := base
	hostCarriesThePort.Endpoint = SourceEndpoint{Kind: EndpointSFTP, User: "backup", Host: "example.internal:2222"}

	portIsItsOwnField := base
	portIsItsOwnField.Endpoint = SourceEndpoint{Kind: EndpointSFTP, User: "backup", Host: "example.internal", Port: 2222}

	if got, other := mustIdentity(t, hostCarriesThePort), mustIdentity(t, portIsItsOwnField); got == other {
		t.Errorf("host %q on the default port and host %q on port %d both produced %s; the port separator is being read out of the host's own value",
			hostCarriesThePort.Endpoint.Host, portIsItsOwnField.Endpoint.Host, portIsItsOwnField.Endpoint.Port, got)
	}
}

// TestSourceIdentity_DiffersAcrossSetsAndSources is the other direction,
// and the one a naive "hash the relative path" implementation gets wrong:
// two backup sets pointed at the same directory, and one set pointed at two
// directories, must never collide.
func TestSourceIdentity_DiffersAcrossSetsAndSources(t *testing.T) {
	t.Parallel()

	const otherSet = "0c9a44e2-77b1-4d3f-8a61-5f0ab2c3d4e5"

	base := SourceIdentityInput{
		SetUUID:  setUUID,
		Endpoint: SourceEndpoint{Kind: EndpointLocal},
		Root:     SourceRoot{Path: "/srv/data/uploads"},
	}
	want := mustIdentity(t, base)

	sameRootOtherSet := base
	sameRootOtherSet.SetUUID = otherSet
	if got := mustIdentity(t, sameRootOtherSet); got == want {
		t.Error("two backup sets over the same directory share one source identity; their snapshots would interleave into a single lineage and retention would prune one set's restore points on the other's policy")
	}

	otherRootSameSet := base
	otherRootSameSet.Root = SourceRoot{Path: "/srv/data/database"}
	if got := mustIdentity(t, otherRootSameSet); got == want {
		t.Error("one backup set over two different directories produced one source identity")
	}

	// A relative root that is equal AFTER prefix stripping but under a
	// different set is still a different source, which is the case that
	// makes the set identifier load-bearing rather than decorative.
	prefixed := SourceIdentityInput{
		SetUUID:  otherSet,
		Endpoint: SourceEndpoint{Kind: EndpointLocal},
		Root:     SourceRoot{Path: "/mnt/disk1/srv/data/uploads", MountPrefix: "/mnt/disk1"},
	}
	if got := mustIdentity(t, prefixed); got == want {
		t.Error("a different set with the same relative root shares an identity")
	}
}

// TestSourceIdentity_IsDeterministicAcrossProcesses pins the actual bytes.
//
// This is the one test in this file that can fail for a "harmless" reason,
// and it is here precisely because that reason is not harmless. Changing
// the canonical form -- the order of the fields, the separator, the hash,
// the prefix -- silently re-identifies every source in every deployment on
// the next upgrade: no predecessor is found, every source is re-read in
// full, storage doubles and the history of every set restarts. If this test
// fails, the change under it is a migration, not a refactor.
//
// The value below was re-pinned once, before any of this was released: the
// #826 review found that the endpoint was hashed as one rendered string, so
// two different endpoints could collide (see
// TestSourceIdentity_EndpointSubFieldsCannotBleedIntoEachOther), and
// splitting it into four fields changed every digest. identitySchema stays
// at v1 deliberately, because a schema tag exists to make a migration
// visible to deployments that have identities stored, and nothing in this
// product writes one yet -- there is no lineage in the field to fork. The
// next change to this form does not get that excuse.
func TestSourceIdentity_IsDeterministicAcrossProcesses(t *testing.T) {
	t.Parallel()

	got := mustIdentity(t, SourceIdentityInput{
		SetUUID:  setUUID,
		Endpoint: SourceEndpoint{Kind: EndpointSFTP, Host: "production.example.internal", User: "backup"},
		Root:     SourceRoot{Path: "/backups/postgres"},
	})

	const want = "759d5e29091c6643655d328bfb422f6bd22362b745d3649b8e928f2d50b0950b"
	if got.String() != want {
		t.Errorf("the canonical source identity changed:\n  got  %s\n  want %s\n"+
			"if this change is intended it re-identifies every source in every existing deployment, which re-reads and re-stores all of them", got, want)
	}
}

// TestSourceIdentity_RefusesWhatItCannotIdentify covers the inputs where
// answering at all would be worse than failing: a source identified by
// nothing, an endpoint kind nobody modelled, a mount prefix that is not
// actually a prefix of the path.
//
// The prefix case is the interesting one. Silently ignoring a prefix that
// does not match would produce an identity that includes the whole mount
// path, which is exactly the value this type exists to keep out, and it
// would do it for the one deployment that tried hardest to get this right.
func TestSourceIdentity_RefusesWhatItCannotIdentify(t *testing.T) {
	t.Parallel()

	for name, in := range map[string]SourceIdentityInput{
		"no set identifier": {
			Endpoint: SourceEndpoint{Kind: EndpointLocal},
			Root:     SourceRoot{Path: "/srv/data"},
		},
		"whitespace in the set identifier": {
			SetUUID:  " " + setUUID,
			Endpoint: SourceEndpoint{Kind: EndpointLocal},
			Root:     SourceRoot{Path: "/srv/data"},
		},
		"no endpoint kind": {
			SetUUID: setUUID,
			Root:    SourceRoot{Path: "/srv/data"},
		},
		"unknown endpoint kind": {
			SetUUID:  setUUID,
			Endpoint: SourceEndpoint{Kind: SourceEndpointKind("s3")},
			Root:     SourceRoot{Path: "/srv/data"},
		},
		"a local endpoint carrying a host": {
			SetUUID:  setUUID,
			Endpoint: SourceEndpoint{Kind: EndpointLocal, Host: "somewhere"},
			Root:     SourceRoot{Path: "/srv/data"},
		},
		"an sftp endpoint with no host": {
			SetUUID:  setUUID,
			Endpoint: SourceEndpoint{Kind: EndpointSFTP, User: "backup"},
			Root:     SourceRoot{Path: "/srv/data"},
		},
		"no root": {
			SetUUID:  setUUID,
			Endpoint: SourceEndpoint{Kind: EndpointLocal},
		},
		"a relative root": {
			SetUUID:  setUUID,
			Endpoint: SourceEndpoint{Kind: EndpointLocal},
			Root:     SourceRoot{Path: "data/uploads"},
		},
		"a root that escapes its prefix": {
			SetUUID:  setUUID,
			Endpoint: SourceEndpoint{Kind: EndpointLocal},
			Root:     SourceRoot{Path: "/srv/data", MountPrefix: "/mnt/user"},
		},
		"a prefix that matches only as text": {
			SetUUID:  setUUID,
			Endpoint: SourceEndpoint{Kind: EndpointLocal},
			Root:     SourceRoot{Path: "/mnt/userdata/uploads", MountPrefix: "/mnt/user"},
		},
		"a relative prefix": {
			SetUUID:  setUUID,
			Endpoint: SourceEndpoint{Kind: EndpointLocal},
			Root:     SourceRoot{Path: "/mnt/user/uploads", MountPrefix: "mnt/user"},
		},
	} {
		if got, err := NewSourceIdentity(in); err == nil {
			t.Errorf("%s: NewSourceIdentity returned %s with no error", name, got)
		}
	}

	// The prefix-as-text case deserves its own assertion on the message,
	// because "/mnt/user" looking like a prefix of "/mnt/userdata" is a
	// mistake an operator will make and the refusal has to explain it.
	_, err := NewSourceIdentity(SourceIdentityInput{
		SetUUID:  setUUID,
		Endpoint: SourceEndpoint{Kind: EndpointLocal},
		Root:     SourceRoot{Path: "/mnt/userdata/uploads", MountPrefix: "/mnt/user"},
	})
	if err == nil {
		t.Fatal("a mount prefix that matches only as text was accepted")
	}
	for _, want := range []string{"/mnt/userdata/uploads", "/mnt/user"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not mention %q", err, want)
		}
	}
}

// TestSourceRoot_RelativeIsTheSourcesOwnPath documents what the identity is
// actually computed over, because a reader of the hash cannot see it: the
// path as the SOURCE sees it, with this deployment's way of reaching it
// removed.
func TestSourceRoot_RelativeIsTheSourcesOwnPath(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		root SourceRoot
		want string
	}{
		{SourceRoot{Path: "/backups/postgres"}, "backups/postgres"},
		{SourceRoot{Path: "/mnt/user/appdata/pg", MountPrefix: "/mnt/user/appdata"}, "pg"},
		{SourceRoot{Path: "/mnt/user/appdata/pg/", MountPrefix: "/mnt/user/appdata/"}, "pg"},
		// The root IS the mount: a whole volume backed up as one source.
		{SourceRoot{Path: "/mnt/user/appdata", MountPrefix: "/mnt/user/appdata"}, "."},
	} {
		got, err := tc.root.Relative()
		if err != nil {
			t.Errorf("SourceRoot%+v.Relative(): %v", tc.root, err)
			continue
		}
		if got != tc.want {
			t.Errorf("SourceRoot%+v.Relative() = %q, want %q", tc.root, got, tc.want)
		}
	}
}

func mustIdentity(t *testing.T, in SourceIdentityInput) SourceIdentity {
	t.Helper()

	id, err := NewSourceIdentity(in)
	if err != nil {
		t.Fatalf("NewSourceIdentity(%+v): %v", in, err)
	}
	if id.IsZero() {
		t.Fatalf("NewSourceIdentity(%+v) returned the zero identity with no error", in)
	}

	return id
}
