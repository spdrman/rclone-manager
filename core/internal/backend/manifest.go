package backend

// Role names what a backend IS to this engine, as opposed to which rclone
// backend it dials. The set is closed IN GO and a manifest may not add to
// it: see doc.go, "How the closed set stays closed".
type Role string

const (
	// RoleObjectStore is a remote object store reached over the network,
	// addressed by a bucket and a key.
	RoleObjectStore Role = "object_store"

	// RoleLocalVolume is a directory on a filesystem this host can see.
	RoleLocalVolume Role = "local_volume"

	// RoleRemoteFilesystem is a directory tree on ANOTHER host, reached
	// over the network and addressed by a path rather than by a bucket
	// and a key. sftp (issue #731) is the first one.
	//
	// It is its own role rather than a second spelling of
	// RoleLocalVolume, and the difference is the whole point: internal/
	// app's mediumType dispatches on this, RoleLocalVolume resolves to
	// transport.MediumTypeLocalDir, and a remote destination that
	// resolved to that would write every "offsite" copy to a directory
	// on this machine. A role internal/app does not yet dial refuses at
	// the moment something is about to be reached, which is that
	// function's own documented answer and the safe one.
	RoleRemoteFilesystem Role = "remote_filesystem"
)

// validRoles is the closed set Role accepts, checked at load. A map
// rather than a switch so validate.go and any future listing (an error
// message naming every accepted role) read it from one place.
var validRoles = map[Role]bool{
	RoleObjectStore:      true,
	RoleLocalVolume:      true,
	RoleRemoteFilesystem: true,
}

// FieldKind is what one collected value IS. The set is closed in Go for
// Role's reason, and there is deliberately no kind that produces a
// duration, a timestamp or a count: see doc.go on FR-32.
type FieldKind string

const (
	KindString     FieldKind = "string"
	KindPath       FieldKind = "path"
	KindURL        FieldKind = "url"
	KindEnum       FieldKind = "enum"
	KindBool       FieldKind = "bool"
	KindCredential FieldKind = "credential"

	// KindKeyPrefix is a key namespace inside a destination: the
	// `prefix` both bundled backends already have. It is its own kind
	// rather than a KindString with a Pattern because its rules are not
	// expressible as one regex - no leading or trailing "/", no empty
	// segment, and no "." or ".." segment - and because the last of
	// those is not a tidiness rule: a key is not only ever a key, since
	// restoring an artifact writes it to a local path derived from the
	// key (config.validateMediumPrefix's own doc, validate.go:1579).
	// Writing that as a regex would be a second, weaker statement of a
	// rule this repository has already argued out once.
	KindKeyPrefix FieldKind = "key_prefix"
)

// validFieldKinds is the closed set FieldKind accepts, checked at load.
var validFieldKinds = map[FieldKind]bool{
	KindString:     true,
	KindPath:       true,
	KindURL:        true,
	KindEnum:       true,
	KindBool:       true,
	KindCredential: true,
	KindKeyPrefix:  true,
}

// EnumValue is one choice a KindEnum field offers: the value that is
// stored, and the words a surface renders for it.
type EnumValue struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// Field is one thing an operator is asked for when they configure an
// instance of this backend.
type Field struct {
	// ID is the key this value is stored under. lower_snake_case, unique
	// within the manifest, and it is deliberately the same spelling the
	// config schema already uses for the same fact (bucket, prefix,
	// region, endpoint, storage_class, upload_verification), which is
	// what #667's expressibility pin checks.
	ID string `json:"id"`

	// Label and Help are what a surface renders. Help is optional.
	Label string `json:"label"`
	Help  string `json:"help,omitempty"`

	Kind     FieldKind `json:"kind"`
	Required bool      `json:"required"`

	// Values is the closed choice set, for KindEnum and only for
	// KindEnum. Non-empty, no duplicate Value.
	Values []EnumValue `json:"values,omitempty"`

	// Pattern is an RE2 the value must match, for KindString and only for
	// KindString. Compiled at load; one that does not compile is a
	// refusal, not a pattern that silently matches nothing.
	Pattern string `json:"pattern,omitempty"`

	// UnsetMeans is the value an ACCESSOR resolves this field to when the
	// instance leaves it empty. It is emphatically not a default written
	// into the instance record: config.StorageMedium.EffectiveStorageClass
	// exists as an accessor rather than a value Validate fills in because
	// "a default written back into the struct is a default frozen into the
	// operator's file by the next settings save" (issue #294), and this
	// field is that argument expressed as data. Nothing in this package
	// writes it anywhere.
	UnsetMeans string `json:"unset_means,omitempty"`
}

// ProbeStep is one step of mediumcheck's closed, ordered vocabulary, and
// whether this backend runs it.
type ProbeStep struct {
	Step string `json:"step"`
	Run  bool   `json:"run"`

	// Reason is required when Run is false, and forbidden when it is
	// true. It is the sentence a surface renders beside a skipped step.
	// mediumcheck's Skipped outcome is a first-class outcome and not a
	// quiet pass, so a step that does not run has to say why in words an
	// operator reads; localcheck.go carries exactly two of these today
	// as Go strings, and this is where they moved to.
	Reason string `json:"reason,omitempty"`
}

// Probe is how an instance of this backend is verified: which of
// mediumcheck's steps apply to it. It declares no procedure. There is no
// script, no command and no expression anywhere in a manifest, which is
// #664's decision and not an omission.
type Probe struct {
	// Steps carries one entry per name in ProbeStepNames, in that exact
	// order. A manifest that omits one, repeats one, names an unknown one
	// or reorders them is refused, so a surface can render a fixed list
	// rather than discovering which ones happened to be declared.
	Steps []ProbeStep `json:"steps"`
}

// Manifest is one registered backend, declared as data.
type Manifest struct {
	// ID is the backend's name: lower_snake_case, unique across the
	// registry, and never ReservedInstanceID.
	ID string `json:"id"`

	// Label and Summary are what the add-a-destination picker renders.
	Label   string `json:"label"`
	Summary string `json:"summary"`

	Role Role `json:"role"`

	// RcloneBackend is the rclone backend an instance of this is dialed
	// through. It must be in SupportedRcloneBackends. This is the FR-4
	// floor: a manifest can never make this process dial something the
	// binary did not register.
	RcloneBackend string `json:"rclone_backend"`

	// Configurable is whether an instance of this backend can be
	// authored TODAY, and it is the one field in this format that is
	// about this BUILD rather than about the backend.
	//
	// A pointer because absent means true: the ordinary manifest says
	// nothing here, and a bool would make "somebody added a manifest
	// and did not think about this" and "this backend cannot be
	// configured" the same document. Read it through IsConfigurable,
	// never directly.
	//
	// False is a manifest shipped as a PREVIEW: the shape is real and
	// is served so an operator searching for it finds an answer, and
	// the layers that would store and dial an instance of it refuse it
	// by name (config.expressibleBackendIDs, service's field mapper,
	// internal/app's mediumType). Registering one without saying so
	// leaves a picker offering a row whose every path ends in a
	// refusal, which is a worse answer than the row that says it is
	// not ready: see doc.go, "A registered backend nothing can
	// configure yet".
	Configurable *bool `json:"configurable,omitempty"`

	// Capabilities is what an instance of this backend can be asked to
	// DO, as opposed to what an operator configures about one (Fields)
	// or what a connection test proves about one (Probe). See
	// capability.go, which owns the vocabulary and the two reasons this
	// is a pointer: absent means UNQUALIFIED rather than "all false",
	// and a block that is present has to answer the whole vocabulary.
	//
	// Read it through DeclaredCapabilities, never directly: that
	// function's zero-value answer is the least capable backend
	// describable, so a caller that forgets to check ok still gets the
	// conservative reading.
	Capabilities *Capabilities `json:"capabilities,omitempty"`

	Fields []Field `json:"fields"`
	Probe  Probe   `json:"probe"`
}

// IsConfigurable reports whether an instance of this backend can be
// authored today. A manifest that says nothing is configurable, so the
// two shipped backends that are stay unchanged files and only the one
// that is not has to declare anything.
func (m Manifest) IsConfigurable() bool {
	return m.Configurable == nil || *m.Configurable
}

// Field returns the field with the given id.
func (m Manifest) Field(id string) (Field, bool) {
	for _, f := range m.Fields {
		if f.ID == id {
			return f, true
		}
	}
	return Field{}, false
}

// CredentialField returns the one KindCredential field, and whether there
// is one. Validation refuses a manifest declaring more than one, so this
// is total: a caller never has to handle "more than one" itself.
func (m Manifest) CredentialField() (Field, bool) {
	for _, f := range m.Fields {
		if f.Kind == KindCredential {
			return f, true
		}
	}
	return Field{}, false
}
