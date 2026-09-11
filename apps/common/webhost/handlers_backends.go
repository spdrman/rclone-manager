package webhost

import (
	"net/http"

	"github.com/backupdproject/backupd/core/service"
)

// This file is EPIC I's (#664) read-only half of the backend registry:
// the route that makes the manifests a build ships reachable from a
// browser.
//
// The registry itself lives in core/internal/backend and is projected
// into shapes this module may see by core/service/backends.go, which is
// also where the argument for why the projection exists at all is
// written. This file is the wire spelling and nothing else.
//
// It exists so #668's add-a-destination wizard can render the backend
// list from the registry instead of from an array of its own. That is not
// a convenience: a picker holding its own list of backend names, or
// switching on backend type, is the assumption EPIC I exists to remove —
// that a destination IS a backend type rather than an instance of one —
// re-asserted in the one surface the epic is about.
//
// There is deliberately no write counterpart, for GET /validators'
// reason one step further on. A manifest decides what a destination may
// BE, including which rclone backend it dials; accepting one over HTTP
// would put FR-4's gate on the far side of the network from the binary it
// is supposed to constrain, and a client could name a backend this build
// does not carry.
//
// # Why a catalogue of manifests is safe to serve
//
// A manifest carries SHAPE and never a value. A credential-kind field
// says a credential is needed here and says nothing about what one is:
// there is no field on any of these shapes that could hold material, and
// handlers_backends_test.go's TestListBackends_DeclaresShapeAndNeverAValue
// asserts that structurally rather than by searching for today's
// secrets. That is #665's C1-C5 doctrine — refusals and disclosures by
// shape, never by content — arriving on the one new API response EPIC I
// adds.

// backendEnumValueResponse is one choice an enum-kind field offers.
type backendEnumValueResponse struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// backendFieldResponse is one thing an operator is asked for when they
// configure an instance. A field-for-field mirror of
// core/service.BackendField.
//
// The three optional strings are omitempty because absent and empty mean
// different things for all three: an absent unset_means says this field
// has no resolved-at-read-time value, and an empty one would read as "it
// resolves to the empty string", which is a different claim.
type backendFieldResponse struct {
	ID         string                     `json:"id"`
	Label      string                     `json:"label"`
	Help       string                     `json:"help,omitempty"`
	Kind       string                     `json:"kind"`
	Required   bool                       `json:"required"`
	Values     []backendEnumValueResponse `json:"values,omitempty"`
	Pattern    string                     `json:"pattern,omitempty"`
	UnsetMeans string                     `json:"unset_means,omitempty"`
}

// backendProbeStepResponse is one step of the verification vocabulary and
// whether this backend runs it.
type backendProbeStepResponse struct {
	Step string `json:"step"`
	Run  bool   `json:"run"`
	// Reason is present exactly when Run is false. A skipped step is a
	// first-class outcome rather than a quiet pass, so the sentence
	// explaining it has to reach the surface that renders the step list.
	Reason string `json:"reason,omitempty"`
}

// backendProbeResponse wraps the step list in an object rather than
// serving a bare array, matching Probe's own shape in the manifest
// format, so a future per-backend probe fact has somewhere to go.
type backendProbeResponse struct {
	Steps []backendProbeStepResponse `json:"steps"`
}

// backendResponse is one registered backend on the wire.
//
// It reports no transport. The manifest format carries one and the
// engine enforces it at load time (backend.SupportedRcloneBackends, the
// FR-4 floor), but that enforcement has already happened or already
// refused the manifest by the time a client reads this, so a copy on the
// wire could enforce nothing and only served as disclosure. #81's
// standing constraint forbids naming an implementation on /api/v1, and
// `role` is the product answer to what a backend is.
type backendResponse struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Summary string `json:"summary"`
	Role    string `json:"role"`

	// Configurable is served always, never omitempty: false is the
	// interesting answer here, and a field that disappeared when it was
	// false would say "this build is too old to know" in exactly the
	// case a client most needs to be told.
	Configurable bool `json:"configurable"`

	Fields []backendFieldResponse `json:"fields"`
	Probe  backendProbeResponse   `json:"probe"`
}

// unregisteredBackendResponse is a storage shape this build understands
// and no manifest declares. An object with one field rather than a bare
// string, so the reason a shape is unregistered can be added later
// without changing the type a client already parses.
//
// Transport, not the manifest id backendResponse carries, because the
// whole point of this row is that no manifest declares it, so there is
// no id to name it by. `sftp` is a protocol, which is why the field can
// be spelled at product level at all.
type unregisteredBackendResponse struct {
	Transport string `json:"transport"`
}

// listBackendsResponse is GET /api/v1/backends' body: an object with
// array fields, matching listValidatorsResponse and
// listBackupSetsResponse, so a future field can be added without
// breaking a client parsing a bare top-level array.
//
// The two naming rules travel with the catalogue rather than in a
// separate settings read, for the reason RetentionSchema's
// tier_name_pattern already establishes: the add-a-destination form has
// to refuse exactly what a hand-edited configuration file would be
// refused for, and one response carrying both the choices and the rules
// means a form cannot be written against a stale copy of either.
type listBackendsResponse struct {
	Backends           []backendResponse             `json:"backends"`
	Unregistered       []unregisteredBackendResponse `json:"unregistered"`
	InstanceIDPattern  string                        `json:"instance_id_pattern"`
	ReservedInstanceID string                        `json:"reserved_instance_id"`
}

// listBackends is GET /api/v1/backends: read-only
// (docs/EPIC-B-multi-nas.md §50's "view configuration" bucket), so no
// CSRF and no destructive gate, exactly like GET /validators alongside
// it. Authentication is not optional and is not this handler's business
// either: router.go applies it to the whole /api/v1 group, and
// TestNoAPIRouteBypassesAuthentication walks every route to prove it.
//
// It takes no backend call. The catalogue is embedded at build time and
// memoised for the life of the process, so there is nothing
// per-deployment to ask a BackupServiceClient for, and adding a method to
// that interface purely to forward a constant would make the seam wider
// for nothing — which is handlers_validators.go's own argument for the
// same shape.
//
// The error path exists for one cause: a manifest this build ships is
// malformed. That is a build fault rather than anything an operator
// configured, which is why it is INTERNAL and not a 400, and why the
// whole registry is refused rather than the well-formed manifests being
// served with the bad one omitted.
func (h *handlers) listBackends(w http.ResponseWriter, r *http.Request) {
	catalog, err := service.RegisteredBackends()
	if err != nil {
		h.internalError(w, r, "INTERNAL", "failed to read the backend registry", err)
		return
	}

	resp := listBackendsResponse{
		Backends:           make([]backendResponse, 0, len(catalog.Backends)),
		Unregistered:       make([]unregisteredBackendResponse, 0, len(catalog.Unregistered)),
		InstanceIDPattern:  catalog.InstanceIDPattern,
		ReservedInstanceID: catalog.ReservedInstanceID,
	}
	for _, b := range catalog.Backends {
		out := backendResponse{
			ID:           b.ID,
			Label:        b.Label,
			Summary:      b.Summary,
			Role:         b.Role,
			Configurable: b.Configurable,
			Fields:       make([]backendFieldResponse, 0, len(b.Fields)),
			Probe:        backendProbeResponse{Steps: make([]backendProbeStepResponse, 0, len(b.Probe))},
		}
		for _, f := range b.Fields {
			field := backendFieldResponse{
				ID:         f.ID,
				Label:      f.Label,
				Help:       f.Help,
				Kind:       f.Kind,
				Required:   f.Required,
				Pattern:    f.Pattern,
				UnsetMeans: f.UnsetMeans,
			}
			if len(f.Values) > 0 {
				field.Values = make([]backendEnumValueResponse, 0, len(f.Values))
				for _, v := range f.Values {
					field.Values = append(field.Values, backendEnumValueResponse{Value: v.Value, Label: v.Label})
				}
			}
			out.Fields = append(out.Fields, field)
		}
		for _, s := range b.Probe {
			out.Probe.Steps = append(out.Probe.Steps, backendProbeStepResponse{Step: s.Step, Run: s.Run, Reason: s.Reason})
		}
		resp.Backends = append(resp.Backends, out)
	}
	for _, name := range catalog.Unregistered {
		resp.Unregistered = append(resp.Unregistered, unregisteredBackendResponse{Transport: name})
	}

	writeJSON(w, http.StatusOK, resp)
}
