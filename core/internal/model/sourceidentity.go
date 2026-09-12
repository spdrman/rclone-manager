// What makes two runs, days apart, agree that they are looking at the same
// source.
//
// An incremental engine's whole value is that it finds the previous
// snapshot of the same source and reuses whatever content still matches.
// What it uses to find it is an identity, and the failure mode of getting
// that identity wrong is silent in both directions:
//
//   - an identity that contains anything about HOW this deployment reaches
//     the source forks the lineage the first time that changes. A disk is
//     replaced, a share is renamed, a container's bind mount moves, an
//     externally-provided snapshot is mounted under a fresh temporary
//     directory every night: the next run finds no predecessor, re-reads
//     every byte, stores a second full copy, and every retention,
//     last-known-good and verification decision downstream is being made
//     about two streams that are the same data. Nothing fails. The
//     deployment silently doubles and loses its history.
//
//   - an identity that is too coarse merges two sources into one lineage,
//     where each run looks like the other one's tree having changed
//     completely, every run re-reads everything, and one set's retention
//     prunes the other's restore points.
//
// So the identity is a deterministic function of three things and nothing
// else: the backup set's DURABLE identifier, the source ENDPOINT, and the
// source root AS THE SOURCE SEES IT.
//
// # Why the set's durable identifier and not its name
//
// A backup set's configured identity is source-plus-name (FR-7), and both
// halves are editable in the UI. If a lineage hung off them, renaming a set
// would orphan every snapshot it had ever taken. So the input is an
// identifier nothing renames, minted once when the set is created, and this
// package takes it as an opaque string: it is not this type's business
// whether that identifier is a uuid, only that it is stable and unique.
//
// # Why the mount prefix is declared rather than detected
//
// Nothing on this side can tell which leading part of /srv/snap-47/data/pg
// is "where the snapshot happened to be mounted" and which is "the source's
// own path". That is a fact about the operator's arrangement, exactly like
// ConsistencyMode, so it is declared. The alternative -- guessing, by
// stripping a temp-looking prefix or by walking up to a mount point -- would
// be a heuristic deciding whether a deployment keeps its backup history.

package model

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"strconv"
	"strings"
)

// identitySchema is the version tag mixed into every identity.
//
// It is here so that a future, deliberate change to what an identity is
// computed over can be a MIGRATION rather than an accident: a new tag
// re-identifies every source, which is exactly the observable consequence,
// and the tests pin the current value so the change cannot happen by
// refactor.
const identitySchema = "backupd.source-identity.v1"

// SourceEndpointKind names the kind of endpoint a source is reached
// through. It is a closed set, like backupengine.LocationKind and for the
// same reason: the default port below is per kind, so a kind this package
// has not been taught cannot be canonicalised, and guessing would put a
// deployment's lineage on a coin flip.
type SourceEndpointKind string

const (
	// EndpointLocal is a path on a locally reachable filesystem, which
	// includes an already-mounted network share and a bind mount. It has
	// no host, user or port: the identity of such a source is entirely its
	// path, which is why the mount prefix matters most here.
	EndpointLocal SourceEndpointKind = "local"

	// EndpointSFTP is a host reached over SSH. Host and user are part of
	// the identity, because the same pathname on two hosts is two
	// different sources and treating them as one would make each run look
	// like the other's tree having been replaced.
	EndpointSFTP SourceEndpointKind = "sftp"
)

// defaultPort is the port a kind uses when configuration names none. It is
// what makes "port: 22" and an omitted port one endpoint rather than two
// lineages, which is a real edit: the wizard writes the port out, a
// hand-written config usually does not.
func (k SourceEndpointKind) defaultPort() (int, bool) {
	switch k {
	case EndpointLocal:
		return 0, true
	case EndpointSFTP:
		return 22, true
	default:
		return 0, false
	}
}

// SourceEndpoint is the identity of the place a source is reached, stripped
// of everything that is credentials or tuning: no key, no known-hosts file,
// no connection limit. Those change without the source changing.
type SourceEndpoint struct {
	Kind SourceEndpointKind
	Host string
	Port int
	User string
}

// canonicalEndpoint is a SourceEndpoint reduced to the four values an
// identity is computed over, each already normalised.
//
// It is four separate values rather than one rendered string on purpose.
// An endpoint written out as "kind://user@host:port" and hashed as a single
// field lets a value reach across a field boundary: user "a@b" on host "c"
// and user "a" on host "b@c" render the same bytes, which is two different
// sources sharing one snapshot lineage (#826). Keeping the parts apart
// until they are length-prefixed individually removes the boundary a value
// could impersonate.
type canonicalEndpoint struct {
	kind string
	user string
	host string
	port string
}

// canonical normalises the endpoint into the parts an identity is computed
// over, and refuses an endpoint it cannot canonicalise.
//
// Two normalisations, both of which are a spelling change rather than an
// identity change, and both of which a real config edit produces:
//
//   - the host is lower-cased, because DNS is case-insensitive and
//     "Production.example.internal" is the same machine;
//   - a zero port becomes the kind's default, because an omitted port and
//     the default written out are the same endpoint.
//
// A local endpoint carrying a host, port or user is refused rather than
// ignored. Ignoring it would mean two configs that differ visibly produce
// one identity, and the operator who wrote the host believed something
// about where their data comes from.
func (e SourceEndpoint) canonical() (canonicalEndpoint, error) {
	port, known := e.Kind.defaultPort()
	if !known {
		if e.Kind == "" {
			return canonicalEndpoint{}, fmt.Errorf("source endpoint has no kind (expected %q or %q)", EndpointLocal, EndpointSFTP)
		}

		return canonicalEndpoint{}, fmt.Errorf("unknown source endpoint kind %q (expected %q or %q)", e.Kind, EndpointLocal, EndpointSFTP)
	}

	if e.Port != 0 {
		port = e.Port
	}

	host := strings.ToLower(strings.TrimSpace(e.Host))
	if host != strings.ToLower(e.Host) {
		return canonicalEndpoint{}, fmt.Errorf("source endpoint host %q has surrounding whitespace", e.Host)
	}

	switch e.Kind {
	case EndpointLocal:
		if host != "" || e.User != "" || e.Port != 0 {
			return canonicalEndpoint{}, fmt.Errorf("a %q source endpoint has no host, user or port, but this one names host=%q user=%q port=%d",
				EndpointLocal, e.Host, e.User, e.Port)
		}
	case EndpointSFTP:
		if host == "" {
			return canonicalEndpoint{}, fmt.Errorf("a %q source endpoint requires a host", EndpointSFTP)
		}
		if e.User == "" {
			return canonicalEndpoint{}, fmt.Errorf("a %q source endpoint requires a user", EndpointSFTP)
		}
	}

	return canonicalEndpoint{
		kind: string(e.Kind),
		user: e.User,
		host: host,
		port: strconv.Itoa(port),
	}, nil
}

// SourceRoot is where the tree being backed up starts, in two parts,
// because only one of them is part of what the source IS.
type SourceRoot struct {
	// Path is the root as this configuration names it: the remote path of
	// a backup set.
	Path string

	// MountPrefix is the leading part of Path that is an artifact of how
	// this deployment reached the source rather than part of the source's
	// own identity: a container bind mount, an install prefix, or the
	// temporary directory an externally-provided snapshot is mounted under
	// for the duration of one run.
	//
	// Empty is the ordinary case, and it means "the whole path is the
	// source's own", which is right for an sftp source whose path is
	// absolute on the far side and does not move when anything on this
	// side does.
	MountPrefix string
}

// Relative is the root as the source sees it: Path with MountPrefix
// removed, cleaned, and without a leading separator.
//
// It refuses rather than tolerates three things, and the last is the one
// worth reading twice:
//
//   - a relative Path, because a source root that is not anchored is not an
//     identity;
//   - a relative MountPrefix, for the same reason;
//   - a MountPrefix that is not a SEGMENT prefix of Path. "/mnt/user" is
//     not a prefix of "/mnt/userdata" however much it looks like one, and
//     the string comparison that says otherwise is the same bug
//     config.commonAncestorDir already exists to avoid. Silently ignoring a
//     prefix that does not match would put the whole mount path back into
//     the identity, for the one deployment that tried hardest to get this
//     right.
func (r SourceRoot) Relative() (string, error) {
	if r.Path == "" {
		return "", fmt.Errorf("source root has no path")
	}

	full := path.Clean(strings.TrimSpace(r.Path))
	if !strings.HasPrefix(full, "/") {
		return "", fmt.Errorf("source root %q is not an absolute path", r.Path)
	}

	if r.MountPrefix == "" {
		return strings.TrimPrefix(full, "/"), nil
	}

	prefix := path.Clean(strings.TrimSpace(r.MountPrefix))
	if !strings.HasPrefix(prefix, "/") {
		return "", fmt.Errorf("source mount prefix %q is not an absolute path", r.MountPrefix)
	}

	if full == prefix {
		// The whole mount is the source: a volume backed up as one tree.
		// "." rather than "" so the value is a path either way, and so an
		// identity over it cannot collide with an empty field.
		return ".", nil
	}

	if !strings.HasPrefix(full, strings.TrimSuffix(prefix, "/")+"/") {
		return "", fmt.Errorf("source root %q is not inside its mount prefix %q", r.Path, r.MountPrefix)
	}

	return strings.TrimPrefix(full, strings.TrimSuffix(prefix, "/")+"/"), nil
}

// SourceIdentityInput is everything a source identity is a function of.
// Nothing else may be added to it without accepting that the value changes
// for every existing deployment: see identitySchema.
type SourceIdentityInput struct {
	// SetUUID is the backup set's durable identifier, opaque here. It is
	// case-folded before hashing, because the same identifier written in
	// two cases is one identifier and a config edit is exactly where that
	// happens.
	SetUUID string

	Endpoint SourceEndpoint
	Root     SourceRoot
}

// SourceIdentity is the stable identity of one backup set's source: a
// hex-encoded digest of the canonical form of a SourceIdentityInput.
//
// It is a digest rather than a readable composite for two reasons. The
// composite would contain a host and a path, and this product already has
// deployments where both are treated as sensitive (config's
// sensitive_endpoint, issue #295), and this value is destined for a
// repository's own source namespace, a catalog column and log lines. And a
// composite invites parsing, which would create a second, unvalidated way
// to build one.
type SourceIdentity string

func (s SourceIdentity) String() string { return string(s) }

// IsZero reports whether this identity is unset, which is what a backup set
// running the artifact engine carries: that engine has no snapshot lineage
// to keep stable.
func (s SourceIdentity) IsZero() bool { return s == "" }

// NewSourceIdentity computes the identity, or refuses an input it cannot
// identify honestly.
//
// The canonical form is a fixed sequence of labelled, length-prefixed
// fields, one per input value, rendered into a single buffer and hashed
// once.
//
// Both halves of that sentence are load bearing:
//
//   - the length prefix is what makes the concatenation unambiguous. A set
//     identifier ending in a separator and a user beginning with one would
//     otherwise produce the same bytes as a different pair.
//   - one field per VALUE, never one field per struct. The endpoint's kind,
//     user, host and port are four fields, because rendering them as
//     "kind://user@host:port" first puts a separator inside the field where
//     a value can impersonate it: user "a@b" on host "c" and user "a" on
//     host "b@c" hashed identically before #826, which is two different
//     sources sharing one snapshot lineage.
//
// The label is hashed alongside its value so that a field added later
// cannot be made to look like an existing one by taking its position.
func NewSourceIdentity(in SourceIdentityInput) (SourceIdentity, error) {
	setID := strings.ToLower(in.SetUUID)
	if setID == "" {
		return "", fmt.Errorf("source identity requires the backup set's durable identifier")
	}

	if setID != strings.TrimSpace(setID) {
		return "", fmt.Errorf("backup set identifier %q has surrounding whitespace", in.SetUUID)
	}

	endpoint, err := in.Endpoint.canonical()
	if err != nil {
		return "", err
	}

	root, err := in.Root.Relative()
	if err != nil {
		return "", err
	}

	fields := [...][2]string{
		{"schema", identitySchema},
		{"set", setID},
		{"endpoint.kind", endpoint.kind},
		{"endpoint.user", endpoint.user},
		{"endpoint.host", endpoint.host},
		{"endpoint.port", endpoint.port},
		{"root", root},
	}

	// One buffer, one hash call. Sized up front so an identity computed
	// per backup set per cycle does not grow a buffer four times on the
	// way: the ten bytes per field cover the "=", the ":", the newline and
	// a decimal length of any value this can hold.
	size := 0
	for _, field := range fields {
		size += len(field[0]) + len(field[1]) + 10
	}

	buf := make([]byte, 0, size)
	for _, field := range fields {
		// label=len:value, newline-terminated.
		buf = append(buf, field[0]...)
		buf = append(buf, '=')
		buf = strconv.AppendInt(buf, int64(len(field[1])), 10)
		buf = append(buf, ':')
		buf = append(buf, field[1]...)
		buf = append(buf, '\n')
	}

	sum := sha256.Sum256(buf)

	return SourceIdentity(hex.EncodeToString(sum[:])), nil
}
