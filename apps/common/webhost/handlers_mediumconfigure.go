package webhost

// Configuring a declared destination in its backend's own vocabulary
// (I2.2, issue #669, EPIC I #664).
//
// Three routes beside the storage-medium ones above rather than instead
// of them. The reason is in core/service/mediumconfigure.go's header and
// is worth one sentence here: the request shape those routes take is
// config.StorageMedium's fields, which is to say S3's, so `bucket` is
// required and there is no `path`, and a local volume - the only
// destination a fresh install has - cannot be described in it at all.
// These speak in manifest field ids.
//
// # What crosses this boundary, and what does not
//
// A field VALUE is something an operator typed, and it goes both ways:
// the GET reports what is configured so a form can offer an edit, and
// the two writes take what was filled in.
//
// Credential material does not, in either direction, and the absence is
// structural rather than filtered. There is no field for it on any struct
// in this file: the writes take a credential REFERENCE, minted by
// importStorageCredentials, and the read reports a BOOLEAN - whether one
// exists - which is what a form needs to decide whether the pair may be
// left empty. Not the reference, not its kind, not its path (FR-33, and
// #665's C3, which this file's own test extends onto these routes).
//
// # The routes are not registered yet, on purpose
//
// router.go is untouched by this commit. contract_test.go refuses a
// route the contract does not declare - "an endpoint that exists but is
// not in the contract is an unversioned, undocumented surface" - and
// api/v1/openapi.json plus the two generated modules are under #668's
// regeneration lock while this lands. Registering these three without
// their contract entries would be committing a test this repository is
// right to fail, so the three router lines and the openapi entries land
// together, in one commit, once the lock is released.
//
// # The gating, measured rather than assumed
//
// The GET is a read and carries neither CSRF nor the destructive gate.
// The two writes carry requireCSRF and not the gate, which is what
// updateStorageMedium and preflightStorageMediumCandidate already
// enforce (router.go), and the contract declares exactly that so the
// declared requirement and the enforced one agree - contract_test.go
// checks both directions, and a claim about gating that the route does
// not implement is a claim that passes review and fails in production.

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"

	"github.com/go-chi/chi/v5"

	"github.com/spdrman/backupd/core/service"
)

// mediumFieldValue is one manifest-declared value, as a pair.
//
// A pair list rather than an object keyed by field id, which is #668's
// call and its reason is the generator's: no schema in this contract is
// a map, and the generator models `additionalProperties` as a bool only,
// so a keyed object would arrive in both languages as an untyped blob.
// The list is sorted by field, so one configuration has one body and a
// test asserting on one does not depend on map iteration order.
type mediumFieldValue struct {
	Field string `json:"field"`
	Value string `json:"value"`
}

// mediumConfigurationRequest is the body both writes take.
//
// An unset optional field is ABSENT from Fields rather than present with
// an empty value, and that is the request contract rather than a
// convention: absent is what a manifest's `unset_means` resolves at read
// time, and an empty value written back is a product default frozen into
// the operator's file by the next save (#294).
//
// An EMPTY list is meaningful and is not the same as an absent one.
// These writes replace the whole declared field set, so empty means
// "this instance carries no values" - a real instruction - which is why
// nothing here treats the two as the same thing.
type mediumConfigurationRequest struct {
	// Backend names the manifest this instance is an instance of, and it
	// is what makes the PUT a create as well as a replace. Required when
	// the id is not declared yet; see
	// service.StorageMediumConfiguration.Backend for why there is
	// nothing to derive it from at that moment (P2, issue #669).
	Backend     string                            `json:"backend"`
	Fields      []mediumFieldValue                `json:"fields"`
	Credentials storageMediumCredentialsReference `json:"credentials"`
}

// mediumConfigurationResponse is GET
// /api/v1/storage-mediums/{id}/configuration's body.
//
// CredentialConfigured is the only thing said about the credential, and
// it is a boolean. See this file's header.
type mediumConfigurationResponse struct {
	Fields               []mediumFieldValue `json:"fields"`
	CredentialConfigured bool               `json:"credential_configured"`
}

// getStorageMediumConfiguration is GET
// /api/v1/storage-mediums/{id}/configuration.
//
// A form has to have this before it can offer an edit: the writes below
// replace the whole declared field set, so a form that started empty and
// saved would unset every field the operator did not retype.
func (h *handlers) getStorageMediumConfiguration(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	state, err := h.backend.StorageMediumConfigurationOf(r.Context(), id)
	if err != nil {
		h.writeMediumConfigurationError(w, r, err, "failed to read the storage medium's configuration")
		return
	}

	writeJSON(w, http.StatusOK, mediumConfigurationResponse{
		Fields:               toMediumFieldValues(state.Fields),
		CredentialConfigured: state.CredentialConfigured,
	})
}

// preflightStorageMediumConfiguration is POST
// /api/v1/storage-mediums/{id}/configuration/preflight: prove a
// configuration before it is written.
//
// It writes nothing to the configuration whatever it answers, and a
// destination that does not work is a 200 with `ok` false - a bucket
// that is not there is what an operator configured, not a request that
// broke, exactly as the two preflights above report themselves.
//
// It carries CSRF and not the destructive gate for preflightStorageMedium's
// reasons, argued in full there: the only object it writes is one it
// generated a random key for, and the only object it deletes is that
// same one.
func (h *handlers) preflightStorageMediumConfiguration(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	cfg, ok := h.decodeMediumConfiguration(w, r)
	if !ok {
		return
	}

	result, err := h.backend.PreflightStorageMediumConfiguration(r.Context(), id, cfg)
	if err != nil {
		h.writeMediumConfigurationError(w, r, err, "failed to check the storage medium's configuration")
		return
	}

	writeJSON(w, http.StatusOK, toMediumPreflightResponse(result))
}

// configureStorageMedium is PUT
// /api/v1/storage-mediums/{id}/configuration: the one create for the
// whole add-and-configure flow, and the replace.
//
// One route for both because a destination cannot exist unconfigured. An
// absent required field is refused by Registry.ValidateInstance
// (core/internal/backend/validate.go:228-233), config.Validate delegates
// every per-field rule to it, and both bundled manifests have required
// fields - so the two-phase "declare it now, fill it in later" write is
// not something this schema can express. PUT is create-or-replace, which
// is what PUT means.
//
// The engine runs the same check in front of the write and refuses when
// it fails (#636), which is why a caller's own passing check is not what
// makes this safe: a bucket policy can change between the two. It
// answers with the destination as it now stands, so a caller renders
// what was persisted rather than echoing its own request.
func (h *handlers) configureStorageMedium(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	cfg, ok := h.decodeMediumConfiguration(w, r)
	if !ok {
		return
	}

	medium, err := h.backend.ConfigureStorageMedium(r.Context(), id, cfg)
	if err != nil {
		h.writeMediumConfigurationError(w, r, err, "failed to configure the storage medium")
		return
	}

	writeJSON(w, http.StatusOK, toStorageMediumBody(medium))
}

// decodeMediumConfiguration reads both writes' body. It reports whether
// the caller may proceed, having already answered the request when it
// may not, which is the shape the handlers above already use for a
// decode that can fail two ways.
//
// A repeated field id is refused rather than last-one-wins. A pair list
// can carry one and a map cannot, so this is the cost of the encoding
// #668 chose, and the honest answer to "bucket appears twice with two
// values" is that nobody can say which was meant.
func (h *handlers) decodeMediumConfiguration(
	w http.ResponseWriter,
	r *http.Request,
) (service.StorageMediumConfiguration, bool) {
	var body mediumConfigurationRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "the request body is not the shape this operation takes")
		return service.StorageMediumConfiguration{}, false
	}

	fields := make(map[string]string, len(body.Fields))
	for _, pair := range body.Fields {
		if pair.Field == "" {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "every entry in fields names a field")
			return service.StorageMediumConfiguration{}, false
		}
		if _, repeated := fields[pair.Field]; repeated {
			// The field id is the SCHEMA's own word and not anything the
			// operator typed, so naming it is safe; the value is not
			// echoed (mediumcreds.go's "refusals by shape, never by
			// content").
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST",
				"the field "+pair.Field+" appears twice with different values and nothing can tell which was meant")
			return service.StorageMediumConfiguration{}, false
		}
		if pair.Value == "" {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST",
				"the field "+pair.Field+" carries an empty value; leave an unset field out of the list instead, so what it means when unset stays the manifest's answer")
			return service.StorageMediumConfiguration{}, false
		}
		fields[pair.Field] = pair.Value
	}

	// A credentials block naming nothing is how a caller says "keep the
	// one already configured", which is why this is a value rather than
	// a pointer: an absent block and an empty one mean the same thing
	// here, and two spellings for one instruction is one of them
	// eventually meaning something else.
	return service.StorageMediumConfiguration{
		Backend: body.Backend,
		Fields:  fields,
		Credentials: service.StorageMediumCredentials{
			ID:      body.Credentials.CredentialsID,
			File:    body.Credentials.File,
			Env:     body.Credentials.Env,
			Command: body.Credentials.Command,
		},
	}, true
}

// toMediumFieldValues sorts by field id, so one configuration has one
// body. See mediumFieldValue's own doc.
func toMediumFieldValues(fields map[string]string) []mediumFieldValue {
	out := make([]mediumFieldValue, 0, len(fields))
	for field, value := range fields {
		out = append(out, mediumFieldValue{Field: field, Value: value})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Field < out[j].Field })
	return out
}

// writeMediumConfigurationError maps the three refusals these routes can
// meet. ErrInvalidRequest covers both a field this manager cannot store
// and a manifest rule the values failed, and both are 400s the caller
// can act on: the message names the field, never the value.
func (h *handlers) writeMediumConfigurationError(
	w http.ResponseWriter,
	r *http.Request,
	err error,
	internal string,
) {
	switch {
	case errors.Is(err, service.ErrMediumNotFound):
		writeError(w, http.StatusNotFound, "MEDIUM_NOT_FOUND",
			"this configuration declares no storage medium with that id")
	case errors.Is(err, service.ErrInvalidRequest):
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
	case errors.Is(err, service.ErrStorageMediumNotProven):
		writeError(w, http.StatusConflict, "MEDIUM_CONNECTION_NOT_PROVEN", err.Error())
	default:
		h.internalError(w, r, "INTERNAL", internal, err)
	}
}
