package webhost

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/spdrman/rclone-manager/core/service"
)

// maxCreateBackupSetBodyBytes bounds POST /api/v1/backup-sets' request
// body, the same rationale as maxSubmitOperationBodyBytes
// (handlers_operations.go): generous headroom over any legitimate
// request (a handful of short strings and a small include-pattern list),
// while still bounding how much of a malformed or hostile request this
// handler reads into memory before giving up.
const maxCreateBackupSetBodyBytes = 1 << 20 // 1 MiB

// backupSetSpec is everything it takes to DESCRIBE one backup set: the
// add-backup-set wizard's (#98) Review step, translated into
// service.CreateBackupSetRequest. See that type's own doc for what
// SSHKeyID and KnownHostsLine reference (a prior POST /ssh-keys and
// POST /ssh/host-key-probe call respectively) and why neither carries
// key material or an unverified fingerprint directly.
//
// Nothing in it asks for anything to be RUN, which is what makes it the
// body of two operations rather than one: POST /api/v1/backup-sets folds
// a set into a configuration that already exists, and POST
// /api/v1/system/first-run writes the first configuration there has ever
// been (firstrun.go). They share this type rather than restating it,
// exactly as api/v1/openapi.json's BackupSetSpec is shared by both.
type backupSetSpec struct {
	SourceName         string   `json:"source_name"`
	Name               string   `json:"name"`
	Host               string   `json:"host"`
	Port               int      `json:"port"`
	User               string   `json:"user"`
	SSHKeyID           string   `json:"ssh_key_id"`
	KnownHostsLine     string   `json:"known_hosts_line"`
	RemotePath         string   `json:"remote_path"`
	LocalPath          string   `json:"local_path"`
	Include            []string `json:"include"`
	CompletionStrategy string   `json:"completion_strategy"`
	StableForSeconds   int      `json:"stable_for_seconds"`
	StaleAfterSeconds  int      `json:"stale_after_seconds"`
	// ValidatorID names one entry in the registered application-validator
	// catalog GET /api/v1/validators serves (handlers_validators.go), or
	// is omitted for none. It is an id and never a path: this package
	// could not accept a path here even if a handler wanted to, since
	// core/service is a separate module whose CreateBackupSetRequest has
	// no field for one, and refuses any id outside its own catalog
	// (docs/EPIC-B-multi-nas.md §26 Step 5).
	ValidatorID string `json:"validator_id"`
	Disabled    bool   `json:"disabled"`
	// ReadOnly declares this backup set's remote source read-only from
	// creation (issue #282's "pull from here, never delete here"), set
	// through the wizard/API rather than by hand-editing config.yaml
	// (issue #316). Omitted or false means exactly what every request
	// before this issue already meant: FR-15's delete step runs
	// unchanged.
	ReadOnly bool `json:"read_only"`
}

// backupSetRequest is POST /api/v1/backup-sets' request body: the spec
// above plus the one thing only a create can ask for.
//
// The embedded struct marshals inline, so the wire shape is unchanged
// from when this was one flat type; what changed is that the field below
// is now declared on the operation that honours it and on no other.
type backupSetRequest struct {
	backupSetSpec
	// RunImmediately is the wizard's "Save, enable & run" tier (as
	// opposed to "Save & enable", RunImmediately: false, Disabled:
	// false, or "Save disabled", Disabled: true). See
	// service.CreateBackupSetRequest.RunImmediately's own doc for why
	// this is folded into create rather than a separate endpoint: this
	// issue's scope is exactly the four endpoints named in its own
	// title, and a per-backup-set-scoped run is not one of them.
	RunImmediately bool `json:"run_immediately"`
}

// backupSetResponse is the wire shape of one backup set, shared by
// GET /api/v1/backup-sets, GET /api/v1/backup-sets/{id} and the 201 POST
// /api/v1/backup-sets returns. It carries nothing service.BackupSet does
// not already have.
type backupSetResponse struct {
	ID                 string   `json:"id"`
	SourceName         string   `json:"source_name"`
	Name               string   `json:"name"`
	Host               string   `json:"host"`
	Port               int      `json:"port"`
	User               string   `json:"user"`
	RemotePath         string   `json:"remote_path"`
	LocalPath          string   `json:"local_path"`
	Include            []string `json:"include"`
	CompletionStrategy string   `json:"completion_strategy"`
	// StableForSeconds is the window the "stable" completion strategy
	// waits for, and 0 for every other strategy. Served (issue #350)
	// because an edit surface offering the strategy has to be able to
	// offer the window with it: without this a client could select
	// "stable" and had nothing to send alongside it, which core refuses,
	// so the only possible outcome was a save that failed.
	StableForSeconds int `json:"stable_for_seconds"`
	// ValidatorID is the registered validator this backup set selected,
	// or "" for none. The id only: what it resolves to is a server-side
	// path, and this package never puts one on the wire (see
	// service.SSHKeyRef.KeyFile for the same rule applied to imported
	// keys).
	ValidatorID string `json:"validator_id"`
	Disabled    bool   `json:"disabled"`
	// ReadOnly is the fully-resolved answer (service.BackupSet.ReadOnly):
	// see backupSetSpec.ReadOnly's own doc for what setting it means.
	// Never omitted, the same discipline Disabled above already follows:
	// a caller must be able to tell "not read-only" from "this build does
	// not report it".
	ReadOnly bool `json:"read_only"`
}

func toBackupSetResponse(bs service.BackupSet) backupSetResponse {
	return backupSetResponse{
		ID:                 bs.ID,
		SourceName:         bs.SourceName,
		Name:               bs.Name,
		Host:               bs.Host,
		Port:               bs.Port,
		User:               bs.User,
		RemotePath:         bs.RemotePath,
		LocalPath:          bs.LocalPath,
		Include:            bs.Include,
		CompletionStrategy: bs.CompletionStrategy,
		StableForSeconds:   int(bs.StableFor / time.Second),
		ValidatorID:        string(bs.ValidatorID),
		Disabled:           bs.Disabled,
		ReadOnly:           bs.ReadOnly,
	}
}

// createBackupSetResponse embeds backupSetResponse (its fields marshal
// inline, at the top level) plus, only when the request's
// RunImmediately was set and honoured, the run_cycle Operation it kicked
// off — the same shape POST /api/v1/operations already returns for one,
// so a client parses it identically either way. RunError is the mandatory
// review's M6 fix (PR #155): set only when the backup set itself was
// created successfully but its requested immediate run failed to start —
// still a 201 (the resource this route creates DOES exist now), never
// alongside Operation (at most one of the two is ever non-empty, mirroring
// service.Operation's own Result/Error convention).
type createBackupSetResponse struct {
	backupSetResponse
	Operation *operationResponse `json:"operation,omitempty"`
	RunError  string             `json:"run_error,omitempty"`
}

// listBackupSetsResponse is GET /api/v1/backup-sets' body: an object
// with one array field, not a bare JSON array at the top level, so a
// future field (pagination, a total count) can be added without
// breaking every existing client the way changing a top-level array's
// shape would.
type listBackupSetsResponse struct {
	BackupSets []backupSetResponse `json:"backup_sets"`
}

// createBackupSet is POST /api/v1/backup-sets: issue #146's
// create-backup-set endpoint, the write path the wizard's three Save
// buttons call. State-changing but non-destructive
// (docs/EPIC-B-multi-nas.md §50: "create/edit backup set"), so it is
// CSRF-protected (router.go) but not unconditionally gated behind the
// destructive-ops gate (gate.go) — creating a backup set never touches,
// let alone deletes, remote or local backup data by itself.
//
// # run_immediately IS gated (mandatory review finding M3, PR #155)
//
// body.RunImmediately turns this same call into "also start a
// run_cycle", the exact action requireDestructiveGate exists to block
// (handlers_operations.go's submitOperation, the ONLY route
// router.go wraps in that middleware, exists to run that action too) —
// so this branch is checked against h.gate directly, below, before the
// backend is ever called. This is deliberately NOT route-level
// middleware: a caller that only wants to persist (RunImmediately false,
// the common case, and "Save disabled") must stay unaffected by whether
// #92's gate has been verified yet, matching
// destructiveGateExemptRoutes' own justification
// (router_test.go) for why this route is structurally exempt from
// requireDestructiveGate in the first place.
func (h *handlers) createBackupSet(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxCreateBackupSetBodyBytes)

	var body backupSetRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeDecodeError(w, err, maxCreateBackupSetBodyBytes)
		return
	}

	if body.RunImmediately && !destructiveGatePassed(h.gate) {
		writeDestructiveGateDenied(w)
		return
	}

	req := service.CreateBackupSetRequest{
		SourceName:         body.SourceName,
		Name:               body.Name,
		Host:               body.Host,
		Port:               body.Port,
		User:               body.User,
		SSHKeyID:           body.SSHKeyID,
		KnownHostsLine:     body.KnownHostsLine,
		RemotePath:         body.RemotePath,
		LocalPath:          body.LocalPath,
		Include:            body.Include,
		CompletionStrategy: body.CompletionStrategy,
		ValidatorID:        service.ValidatorID(body.ValidatorID),
		StableFor:          secondsToDuration(body.StableForSeconds),
		StaleAfter:         secondsToDuration(body.StaleAfterSeconds),
		Disabled:           body.Disabled,
		ReadOnly:           body.ReadOnly,
		RunImmediately:     body.RunImmediately,
		Actor:              actorFromContext(r.Context()),
	}

	result, err := h.backend.CreateBackupSet(r.Context(), req)
	if err != nil {
		if result.Set.ID == "" {
			// Creation itself never happened — nothing was persisted, so
			// the ordinary error mapping (400 or 500, per the failure
			// kind: writeBackupSetError has no conflict branch, and this
			// route declares no 409) is the whole story.
			writeBackupSetError(w, err)
			return
		}
		// Mandatory review finding M6 (PR #155): the backup set IS
		// already durably persisted and hot-reloaded at this point (see
		// service.CreateBackupSet's own doc) — only the immediate
		// run_cycle it also requested failed to start. Collapsing this
		// to a bare 500, as if creation itself had failed, is actively
		// misleading: a caller that retries the whole create next hits
		// config.Validate's duplicate-id rejection instead, with no way
		// to tell "already exists because your last attempt actually
		// worked" apart from "your request was wrong from the start".
		// 201, not 500: the resource this route creates was, in fact,
		// created.
		resp := createBackupSetResponse{
			backupSetResponse: toBackupSetResponse(result.Set),
			RunError:          runStartErrorMessage(err),
		}
		writeJSON(w, http.StatusCreated, resp)
		return
	}

	resp := createBackupSetResponse{backupSetResponse: toBackupSetResponse(result.Set)}
	if result.Operation != nil {
		op := toOperationResponse(*result.Operation)
		resp.Operation = &op
	}
	writeJSON(w, http.StatusCreated, resp)
}

// runStartErrorMessage turns the error CreateBackupSet returns when the
// backup set was persisted but its requested immediate run_cycle failed
// to start into a message safe to put on the wire, using the same
// sentinel-to-safe-string classification submitOperation
// (handlers_operations.go) already applies to the identical
// SubmitRunCycle error vocabulary — err here always wraps one of those
// same sentinels (see service.CreateBackupSetRequest.RunImmediately's
// doc), so nothing from a deeper, unclassified layer can reach this far.
func runStartErrorMessage(err error) string {
	switch {
	case errors.Is(err, service.ErrConfigRevisionStale),
		errors.Is(err, service.ErrIdempotencyKeyConflict),
		errors.Is(err, service.ErrOperationAlreadyRunning),
		errors.Is(err, service.ErrInvalidRequest):
		return err.Error()
	default:
		return "the backup set was created, but starting the requested run failed"
	}
}

// listBackupSets is GET /api/v1/backup-sets: read-only (§50), no CSRF, no
// destructive gate — see router.go.
func (h *handlers) listBackupSets(w http.ResponseWriter, r *http.Request) {
	sets, err := h.backend.ListBackupSets(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to list backup sets")
		return
	}
	resp := listBackupSetsResponse{BackupSets: make([]backupSetResponse, 0, len(sets))}
	for _, s := range sets {
		resp.BackupSets = append(resp.BackupSets, toBackupSetResponse(s))
	}
	writeJSON(w, http.StatusOK, resp)
}

// getBackupSet is GET /api/v1/backup-sets/{id}: read-only (§50). id is
// read from chi's "*" wildcard param, not a named {id} segment: a backup
// set's id is "source/name" (model.BackupSetID.String()), and a plain
// chi path parameter never matches a literal "/" within one segment (see
// router.go's route-registration comment for why {id:.*} does not work
// either).
func (h *handlers) getBackupSet(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "*")
	set, err := h.backend.GetBackupSet(r.Context(), id)
	if err != nil {
		if errors.Is(err, service.ErrBackupSetNotFound) {
			writeError(w, http.StatusNotFound, "BACKUP_SET_NOT_FOUND", "no such backup set")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to load backup set")
		return
	}
	writeJSON(w, http.StatusOK, toBackupSetResponse(set))
}

// writeBackupSetError maps a CreateBackupSet (or, via the same sentinels,
// any other backupsets.go method) error to the HTTP status/code this
// package's other handlers already establish the vocabulary for
// (handlers_operations.go's identical switch is the direct precedent).
func writeBackupSetError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrInvalidRequest):
		// Safe to echo: every ErrInvalidRequest this package returns is
		// built from its own field-description strings and the caller's
		// own request values (config.ValidationError, or backupsets.go's
		// own validateCreateRequest), never from state/rclone internals.
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
	case errors.Is(err, service.ErrSSHKeyNotFound):
		writeError(w, http.StatusBadRequest, "SSH_KEY_NOT_FOUND", "the referenced ssh_key_id does not exist; import a key first")
	case errors.Is(err, service.ErrRepointNotAcknowledged):
		// 409 rather than 400, because this is not a malformed request:
		// it is a well-formed one whose consequences the caller has to
		// see first, and it conflicts with the state of the resource
		// (artifacts already on record for this set) rather than with
		// its own shape. A client that could only see "400" could offer
		// an operator nothing better than the same failure again.
		//
		// Safe to echo, on the same terms as ErrInvalidRequest above:
		// core/service builds this message from its own text plus the
		// caller's own path values and a count, never from a state or
		// rclone internal.
		writeError(w, http.StatusConflict, "BACKUP_SET_REPOINT_NOT_ACKNOWLEDGED", err.Error())
	case errors.Is(err, service.ErrConfigNotFileBacked):
		writeError(w, http.StatusInternalServerError, "INTERNAL", "this deployment has no configuration file to persist to")
	default:
		// Deliberately not err.Error(): an unclassified error could carry
		// filesystem or rclone-internal text (see handlers_operations.go's
		// identical default case for the same reasoning).
		//
		// "write" rather than "create": this function serves the update
		// path too (issue #350), and telling an operator whose edit
		// failed that a creation failed sends them looking for a set
		// that was never being created.
		writeError(w, http.StatusInternalServerError, "INTERNAL", "failed to write backup set")
	}
}

// writeDecodeError maps a json.Decoder error the same way submitOperation
// (handlers_operations.go) already does for POST /api/v1/operations,
// factored out here so this file and that one do not each carry their own
// copy of the http.MaxBytesError-vs-anything-else switch.
func writeDecodeError(w http.ResponseWriter, err error, limit int64) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST",
			fmt.Sprintf("request body exceeds the %d byte limit", limit))
		return
	}
	writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "malformed JSON body")
}

// secondsToDuration turns a wire "N seconds" integer into a
// time.Duration; the wizard's JSON body carries seconds (a plain number)
// rather than a Go-duration-string, so the HTTP layer is the one place
// that conversion happens, before service.CreateBackupSetRequest sees a
// real time.Duration like the rest of this codebase already uses.
func secondsToDuration(s int) time.Duration {
	return time.Duration(s) * time.Second
}

// setEnabledRequest is POST /api/v1/backup-sets/{id}/enabled's body.
type setEnabledRequest struct {
	Enabled bool `json:"enabled"`
}

// setBackupSetEnabled is POST /api/v1/backup-sets/{id}/enabled: turn one
// backup set on or off.
//
// It carries requireCSRF and NOT requireDestructiveGate, which puts it in
// the same tier as createBackupSet and updateSettings
// (destructiveGateExemptRoutes, router_test.go). Nothing reachable from
// here touches, moves or deletes a byte of backup data: a disabled set is
// excluded from every run cycle, and everything already backed up stays
// exactly where it is, which core/service pins directly
// (TestSetBackupSetEnabled_DisablingDeletesNothing).
//
// The direction that sounds dangerous is turning a set OFF, because new
// restore points stop being made and freshness decays. That is not hidden:
// FR-24's health computation reports the set going stale, GET
// /api/v1/system/health serves it, and the same call turns it back on.
//
// The id is read from two named segments rather than a catch-all: unlike
// an artifact id this one has a fixed arity of two, and this route needs a
// literal "/enabled" tail after it, which a catch-all would swallow.
func (h *handlers) setBackupSetEnabled(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxCreateBackupSetBodyBytes)

	var body setEnabledRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeDecodeError(w, err, maxCreateBackupSetBodyBytes)
		return
	}

	id := chi.URLParam(r, "source") + "/" + chi.URLParam(r, "set")
	updated, err := h.backend.SetBackupSetEnabled(r.Context(), id, body.Enabled)
	if err != nil {
		if errors.Is(err, service.ErrBackupSetNotFound) {
			writeError(w, http.StatusNotFound, "BACKUP_SET_NOT_FOUND", "no such backup set")
			return
		}
		writeBackupSetError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toBackupSetResponse(updated))
}

// setReadOnlyRequest is POST /api/v1/backup-sets/{id}/read-only's body.
type setReadOnlyRequest struct {
	ReadOnly bool `json:"read_only"`
}

// setBackupSetReadOnly is POST /api/v1/backup-sets/{source}/{set}/read-only
// (issue #316): declare, or withdraw, one already-persisted backup set's
// read-only status without hand-editing config.yaml — the CRUD-parity
// counterpart setBackupSetEnabled already has for `disabled`.
//
// Same tier as setBackupSetEnabled: requireCSRF but NOT
// requireDestructiveGate. Nothing reachable from here touches, moves or
// deletes a byte of backup data either direction. Turning read-only ON
// only PREVENTS a future deletion (service.SetBackupSetReadOnly's own
// doc); turning it back OFF does not reach back and delete anything this
// manager already retained under it, so neither direction is the
// "delete a byte of backup data" requireDestructiveGate exists to gate.
//
// The id is read from two named segments, like setBackupSetEnabled
// beside it, for the identical reason: a backup set id is always exactly
// source/name, a fixed arity, and this route needs a literal
// "/read-only" tail a catch-all would swallow.
func (h *handlers) setBackupSetReadOnly(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxCreateBackupSetBodyBytes)

	var body setReadOnlyRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeDecodeError(w, err, maxCreateBackupSetBodyBytes)
		return
	}

	id := chi.URLParam(r, "source") + "/" + chi.URLParam(r, "set")
	updated, err := h.backend.SetBackupSetReadOnly(r.Context(), id, body.ReadOnly)
	if err != nil {
		if errors.Is(err, service.ErrBackupSetNotFound) {
			writeError(w, http.StatusNotFound, "BACKUP_SET_NOT_FOUND", "no such backup set")
			return
		}
		writeBackupSetError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toBackupSetResponse(updated))
}

// updateBackupSetRequest is PATCH /api/v1/backup-sets/{source}/{set}'s
// body: issue #350's edit surface, and a sparse one.
//
// Every field is a pointer, and that is load-bearing rather than
// stylistic. encoding/json leaves a pointer nil when its key is absent
// and sets it when the key is present, which is what lets this route
// carry "change only remote_path" as a request that is structurally
// incapable of also moving local_path. A value-typed body could not: a
// missing "port" and an explicit "port": 0 would arrive identically, and
// 0 is a real answer here (it selects the default port), so the Web UI's
// per-box Save would end up shipping every other box's current contents
// alongside the one an operator actually pressed Save on.
//
// It deliberately carries no name/source_name, no ssh_key_id and no
// known_hosts_line. See core/service/backupsetupdate.go's own package doc
// for why each of those is not an edit.
type updateBackupSetRequest struct {
	Host       *string   `json:"host"`
	Port       *int      `json:"port"`
	User       *string   `json:"user"`
	RemotePath *string   `json:"remote_path"`
	LocalPath  *string   `json:"local_path"`
	Include    *[]string `json:"include"`

	CompletionStrategy *string `json:"completion_strategy"`
	StableForSeconds   *int    `json:"stable_for_seconds"`
	StaleAfterSeconds  *int    `json:"stale_after_seconds"`

	ValidatorID *string `json:"validator_id"`

	// AcknowledgeRepoint is not a field of the backup set and is not a
	// pointer for that reason: it answers one refusal for one request
	// rather than carrying a stored value. Absent is false, which is the
	// honest reading of a client that did not mention it. See
	// core/service/backupsetrepoint.go for what it acknowledges.
	AcknowledgeRepoint bool `json:"acknowledge_repoint"`
}

// updateBackupSet is PATCH /api/v1/backup-sets/{source}/{set} (issue
// #350): change one already-persisted backup set's definition.
//
// PATCH rather than PUT, and PATCH rather than a POST tail like /enabled
// and /read-only beside it. Those two are single-valued toggles with a
// name; this is a partial edit of a resource, which is what PATCH means,
// and this package already uses it for exactly that shape at PATCH
// /api/v1/settings. A PUT would promise whole-resource replacement, which
// this route deliberately does not offer: a client that sent a PUT
// missing a field would be asking for it to be cleared, and in a backup
// tool that is the kind of promise that quietly empties an include list.
//
// requireCSRF and NOT requireDestructiveGate, following createBackupSet's
// own tier (destructiveGateExemptRoutes, router_test.go). §50 puts
// "create/edit backup set" in one bucket, and nothing reachable from here
// touches, moves or deletes a byte of backup data: the config file
// changes, and the next cycle acts on the new definition. The gate exists
// for run_immediately and for retention apply, not for writing a set.
//
// The id comes from two named segments rather than the catch-all
// getBackupSet uses, exactly like /enabled and /read-only: a backup set
// id is always exactly source/name, a fixed arity, so a route that says
// so lets chi answer a malformed id with a 404 instead of a handler
// having to interpret one.
func (h *handlers) updateBackupSet(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxCreateBackupSetBodyBytes)

	var body updateBackupSetRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeDecodeError(w, err, maxCreateBackupSetBodyBytes)
		return
	}

	req := service.UpdateBackupSetRequest{
		Host:               body.Host,
		Port:               body.Port,
		User:               body.User,
		RemotePath:         body.RemotePath,
		LocalPath:          body.LocalPath,
		Include:            body.Include,
		CompletionStrategy: body.CompletionStrategy,
		StableFor:          secondsPointerToDuration(body.StableForSeconds),
		StaleAfter:         secondsPointerToDuration(body.StaleAfterSeconds),
		AcknowledgeRepoint: body.AcknowledgeRepoint,
	}
	if body.ValidatorID != nil {
		id := service.ValidatorID(*body.ValidatorID)
		req.ValidatorID = &id
	}

	id := chi.URLParam(r, "source") + "/" + chi.URLParam(r, "set")
	updated, err := h.backend.UpdateBackupSet(r.Context(), id, req)
	if err != nil {
		if errors.Is(err, service.ErrBackupSetNotFound) {
			writeError(w, http.StatusNotFound, "BACKUP_SET_NOT_FOUND", "no such backup set")
			return
		}
		writeBackupSetError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toBackupSetResponse(updated))
}

// secondsPointerToDuration is secondsToDuration for a field that has to
// keep telling "absent" apart from "zero". It returns nil for nil, so a
// body that never mentioned stable_for_seconds reaches core/service as a
// nil *time.Duration rather than as a pointer to zero, which
// core/service would read as an operator asking for zero.
func secondsPointerToDuration(s *int) *time.Duration {
	if s == nil {
		return nil
	}
	d := secondsToDuration(*s)
	return &d
}
