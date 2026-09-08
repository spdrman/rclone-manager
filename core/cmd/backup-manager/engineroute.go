package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spdrman/rclone-manager/core/apicontract"
	"github.com/spdrman/rclone-manager/core/internal/apiclient"
	"github.com/spdrman/rclone-manager/core/service"
)

// The one place core/service's vocabulary and core/apicontract's meet.
//
// Everything the two spell differently is in this file and nowhere else,
// which is the property route.go's interface exists to make possible and
// the reason this file is worth reading top to bottom rather than
// dipping into. There are four differences and each one is a decision:
//
// A duration on one side is a whole number of SECONDS on the other
// (stable_for_seconds, stale_after_seconds). Truncating quietly would let
// `--stable-for 1500ms` be persisted as one second by the engine and as a
// second and a half by a direct write, which is two deployments disagreeing
// about a completion rule. So a value the wire cannot carry is refused
// here, before anything is sent.
//
// An ACTOR is a field of the request on one side and a fact about the
// session on the other. service.CreateBackupSetRequest carries
// Actor: cliActor, and the engine records whoever is signed in. The field
// is dropped rather than sent, and that is the routing working rather than
// something lost in it: the whole argument for going through the engine is
// that the engine decides who acted, from a session it minted, instead of
// believing a client that told it.
//
// An IDENTITY is one string on one side and two path segments on the
// other. "source/name" is split here, because api/v1/openapi.json publishes
// /backup-sets/{source}/{set} as two parameters and the client escapes each
// one (PR #546's review found what a single composite parameter cost: every
// real backup set came back not-found).
//
// There used to be a fourth difference and there is not any more. The
// API's BackupSet carried no stale_after at all, so the engine could not
// report what it had just persisted for a field it had accepted on the way
// in, and a routed create printed "not reported" for a value the operator
// had typed on that very command line. #555 put stale_after_seconds on the
// wire, so both windows now cross in both directions and this adapter
// carries no gap at all.

// engineRoute is backupSetRoute over the running engine.
type engineRoute struct {
	client *apiclient.Client
}

// ImportSSHKey hands the key material to the engine's own key store.
func (r *engineRoute) ImportSSHKey(ctx context.Context, raw []byte, passphrase string) (service.SSHKeyRef, error) {
	resp, err := r.client.ImportSSHKey(ctx, apicontract.ImportSSHKeyRequest{
		PrivateKeyPEM: string(raw),
		Passphrase:    passphrase,
	})
	if err != nil {
		return service.SSHKeyRef{}, err
	}
	// KeyFile is deliberately left empty. It is the server-side path the
	// id resolves to, the API does not publish it (SSHKeyRef's own doc
	// gives the rule), and it is not this deployment's filesystem anyway.
	return service.SSHKeyRef{
		ID:          resp.ID,
		Algorithm:   resp.Algorithm,
		Fingerprint: resp.Fingerprint,
	}, nil
}

// ProbeHostKey asks the ENGINE to dial the source and report what
// answered, which is the only end whose answer is worth trusting: the
// engine is the process that will be pulling backups over that connection.
func (r *engineRoute) ProbeHostKey(ctx context.Context, host string, port int) (service.HostKeyProbe, error) {
	resp, err := r.client.ProbeHostKey(ctx, apicontract.HostKeyProbeRequest{Host: host, Port: port})
	if err != nil {
		return service.HostKeyProbe{}, err
	}
	return service.HostKeyProbe{
		Algorithm:      resp.Algorithm,
		Fingerprint:    resp.Fingerprint,
		KnownHostsLine: resp.KnownHostsLine,
	}, nil
}

// CreateBackupSet is POST /backup-sets.
func (r *engineRoute) CreateBackupSet(ctx context.Context, req service.CreateBackupSetRequest) (service.CreateBackupSetResult, error) {
	stableFor, err := wireSeconds(req.StableFor, "--stable-for")
	if err != nil {
		return service.CreateBackupSetResult{}, err
	}
	staleAfter, err := wireSeconds(req.StaleAfter, "--stale-after")
	if err != nil {
		return service.CreateBackupSetResult{}, err
	}

	resp, err := r.client.CreateBackupSet(ctx, apicontract.CreateBackupSetRequest{
		BackupSetSpec: apicontract.BackupSetSpec{
			SourceName:         req.SourceName,
			Name:               req.Name,
			Host:               req.Host,
			Port:               req.Port,
			User:               req.User,
			SSHKeyID:           req.SSHKeyID,
			KnownHostsLine:     req.KnownHostsLine,
			RemotePath:         req.RemotePath,
			LocalPath:          req.LocalPath,
			Include:            req.Include,
			CompletionStrategy: req.CompletionStrategy,
			StableForSeconds:   stableFor,
			StaleAfterSeconds:  staleAfter,
			ValidatorID:        string(req.ValidatorID),
			Disabled:           req.Disabled,
			ReadOnly:           req.ReadOnly,
			// Issue #624: whether this create ran its own check. Carried
			// across because a routed --no-verify that dropped it would
			// write a set the engine reported as proven, which is exactly
			// the indistinguishable-from-verified state the mark exists
			// to prevent.
			ConnectionUnverified: req.ConnectionUnverified,
		},
		RunImmediately:     req.RunImmediately,
		AcknowledgeRepoint: req.AcknowledgeRepoint,
		// req.Actor is NOT sent. See this file's own doc: the engine
		// records the session it minted, and a body that named an actor
		// would be a client asserting an identity.
	})
	if err != nil {
		return service.CreateBackupSetResult{}, err
	}

	result := service.CreateBackupSetResult{Set: backupSetFromWire(resp.BackupSet)}
	if resp.Operation != nil {
		result.Operation = &service.Operation{
			ID:             resp.Operation.OperationID,
			Actor:          resp.Operation.Actor,
			BackupSetID:    resp.Operation.BackupSetID,
			ConfigRevision: resp.Operation.ConfigRevision,
			Action:         resp.Operation.Action,
			Status:         resp.Operation.Status,
			Result:         resp.Operation.Result,
			Error:          resp.Operation.Error,
		}
	}
	if resp.RunError != "" {
		// The set exists and the run the operator asked for did not start.
		// The direct route reports that by returning a nil Operation from
		// a successful create and nothing else, so this says the part the
		// direct route cannot: which is more than it could, not less.
		fmt.Fprintf(os.Stderr, "backup-manager: the set was created and the run it asked for did not start: %s\n", resp.RunError)
	}
	return result, nil
}

// UpdateBackupSet is PATCH /backup-sets/{source}/{set}.
func (r *engineRoute) UpdateBackupSet(ctx context.Context, id string, req service.UpdateBackupSetRequest) (service.BackupSet, error) {
	source, set, ok := splitBackupSetID(id)
	if !ok {
		return service.BackupSet{}, fmt.Errorf("%q is not a backup set id; a backup set id is exactly source/name", id)
	}

	body := apicontract.UpdateBackupSetRequest{
		Host:               req.Host,
		Port:               req.Port,
		User:               req.User,
		RemotePath:         req.RemotePath,
		LocalPath:          req.LocalPath,
		Include:            req.Include,
		CompletionStrategy: req.CompletionStrategy,
		// Issue #572's two, carried across with the same nil/non-nil
		// distinction everything else on this body keeps: a routed patch
		// that dropped them would report success for a rotation the
		// engine never heard about.
		SSHKeyID:       req.SSHKeyID,
		KnownHostsLine: req.KnownHostsLine,

		AcknowledgeRepoint:       req.AcknowledgeRepoint,
		AcknowledgeHostKeyChange: req.AcknowledgeHostKeyChange,
		// Issue #624's opt-out, carried across for the same reason the
		// two acknowledgements are: an edit refused by the engine for a
		// connection it could not prove has one way past it, and a
		// routed --no-verify that dropped this flag would be refused
		// where the direct one succeeds.
		SkipConnectionCheck: req.SkipConnectionCheck,
	}
	if req.ValidatorID != nil {
		v := string(*req.ValidatorID)
		body.ValidatorID = &v
	}
	// Both windows keep the nil/non-nil distinction the whole way across,
	// because that is what a sparse edit means on both sides: "leave this
	// alone" and "set this to zero" are opposite requests, and a mapping
	// that collapsed them would silently clear a field nobody named.
	if req.StableFor != nil {
		seconds, err := wireSeconds(*req.StableFor, "--stable-for")
		if err != nil {
			return service.BackupSet{}, err
		}
		body.StableForSeconds = &seconds
	}
	if req.StaleAfter != nil {
		seconds, err := wireSeconds(*req.StaleAfter, "--stale-after")
		if err != nil {
			return service.BackupSet{}, err
		}
		body.StaleAfterSeconds = &seconds
	}

	updated, err := r.client.UpdateBackupSet(ctx, source, set, body)
	if err != nil {
		return service.BackupSet{}, err
	}
	return backupSetFromWire(updated), nil
}

// TestConnection is POST /backup-sets/test-connection in its CANDIDATE
// mode: prove a source described by a request, before any set exists for
// it (issue #624).
//
// Made by the ENGINE rather than here, for ProbeHostKey's reason restated
// with a sharper edge: `backup-set create` verifies before it writes, so
// the check and the write have to happen in the same world. A route proven
// from this shell and a set declared in a process on the other side of a
// container boundary would be two different claims about two different
// networks, reported as one.
func (r *engineRoute) TestConnection(ctx context.Context, req service.ConnectionTestRequest) (service.ConnectionTestResult, error) {
	return connectionTestFromWire(r.client.TestConnection(ctx, apicontract.TestConnectionRequest{
		Host:           req.Host,
		Port:           req.Port,
		User:           req.User,
		SSHKeyID:       req.SSHKeyID,
		KnownHostsLine: req.KnownHostsLine,
		RemotePath:     req.RemotePath,
	}))
}

// TestBackupSetConnection is the same route in its PERSISTED mode: prove
// the set this id names, by id alone.
//
// The connection details come off the engine's own configuration, which is
// the half that makes this mode worth having: a caller asking whether
// "nas-a/photos" still works neither knows nor has to echo back that set's
// key reference and trusted line, so a read-only check cannot be turned
// into a check of something else.
//
// This is also where a passing check clears issue #624's unverified mark,
// in the process that holds the configuration the mark is in.
func (r *engineRoute) TestBackupSetConnection(ctx context.Context, id string) (service.ConnectionTestResult, error) {
	if !isBackupSetID(id) {
		return service.ConnectionTestResult{}, fmt.Errorf("%q is not a backup set id; a backup set id is exactly source/name", id)
	}
	return connectionTestFromWire(r.client.TestConnection(ctx, apicontract.TestConnectionRequest{BackupSetID: id}))
}

// connectionTestFromWire is the one translation both modes come back
// through, so the routed answer and the direct one cannot describe the
// same six steps differently.
func connectionTestFromWire(resp apicontract.TestConnectionResponse, err error) (service.ConnectionTestResult, error) {
	if err != nil {
		return service.ConnectionTestResult{}, err
	}
	result := service.ConnectionTestResult{OK: resp.OK, Message: resp.Message}
	for _, c := range resp.Checks {
		result.Checks = append(result.Checks, service.ConnectionCheck{
			Step:       c.Step,
			Outcome:    c.Outcome,
			Category:   c.Category,
			Detail:     c.Detail,
			DurationMs: c.DurationMs,
		})
	}
	return result, nil
}

// RemoveBackupSet is DELETE /backup-sets/{source}/{set}.
func (r *engineRoute) RemoveBackupSet(ctx context.Context, id string) error {
	source, set, ok := splitBackupSetID(id)
	if !ok {
		return fmt.Errorf("%q is not a backup set id; a backup set id is exactly source/name", id)
	}
	return r.client.RemoveBackupSet(ctx, source, set)
}

// ErrArtifactsNotRouted is this route saying it cannot answer a question
// rather than answering it wrongly.
//
// `backup-set remove` counts what stays on storage as a courtesy, off the
// journal, and over this route that count is GET /backups with the set in
// a query parameter. The client can make that call, since #544 added
// listArtifacts and the query support it needs. What nothing has written
// is the other half, an apicontract.Artifact turned back into a
// service.Artifact, and #544 did not need one: it asks the engine whether
// it holds the same backups and compares ids (readagreement.go), rather
// than asking it to render the rows this command prints.
//
// So the count is refused, not faked, and backupSetRemoveWith already
// knows what to do with a count it could not take: it says so, in place of
// printing a reassuring 0 about something nothing looked at. The removal
// itself is unaffected, which is the half the operator asked for.
var ErrArtifactsNotRouted = errors.New("this build does not count what stays on storage when a removal goes through a running engine")

// ListArtifacts is the read `backup-set remove` uses for its count.
func (r *engineRoute) ListArtifacts(_ context.Context, _ service.ArtifactFilter) ([]service.Artifact, error) {
	return nil, ErrArtifactsNotRouted
}

// backupSetFromWire turns the engine's answer back into the shape every
// command in this package already prints.
//
// Going back through core/service's own type rather than printing the wire
// shape is what keeps the two routes' output one thing: printBackupSet has
// one input and cannot grow a second rendering for the engine-attached
// case.
func backupSetFromWire(s apicontract.BackupSet) service.BackupSet {
	return service.BackupSet{
		ID:                  s.ID,
		SourceName:          s.SourceName,
		Name:                s.Name,
		Host:                s.Host,
		Port:                s.Port,
		User:                s.User,
		RemotePath:          s.RemotePath,
		LocalPath:           s.LocalPath,
		Include:             s.Include,
		CompletionStrategy:  s.CompletionStrategy,
		StableFor:           time.Duration(s.StableForSeconds) * time.Second,
		StaleAfter:          time.Duration(s.StaleAfterSeconds) * time.Second,
		ValidatorID:         service.ValidatorID(s.ValidatorID),
		Disabled:            s.Disabled,
		ReadOnly:            s.ReadOnly,
		RetentionIsOverride: s.RetentionIsOverride,
		// Issue #624. An engine older than this field answers false,
		// which reads as "nothing here says this set's connection was
		// skipped" rather than as a claim that it was proven, and that
		// is the same reading the configuration file's own absent key
		// gets.
		ConnectionUnverified: s.ConnectionUnverified,
	}
}

// wireSeconds converts a duration to the whole seconds the contract
// carries, refusing anything that would not survive the trip.
//
// Refusing rather than rounding, and refusing HERE rather than letting the
// engine decide, because the failure this prevents is silent: a create with
// a sub-second window would persist one value through the engine and a
// different one directly, and both routes would report success.
func wireSeconds(d time.Duration, flag string) (int, error) {
	if d < 0 {
		return 0, fmt.Errorf("%s is negative, and the engine's API carries it as a whole number of seconds", flag)
	}
	if d%time.Second != 0 {
		return 0, fmt.Errorf("%s is %s, and the engine's API carries it as a whole number of seconds, so this would reach the engine as a different value from the one you typed; give it in whole seconds", flag, d)
	}
	return int(d / time.Second), nil
}

// Settings is GET /settings: the retention and capacity policy the engine
// is actually deciding with, resolved.
func (r *engineRoute) Settings(ctx context.Context) (service.Settings, error) {
	resp, err := r.client.GetSettings(ctx)
	if err != nil {
		return service.Settings{}, err
	}
	return settingsFromWire(resp), nil
}

// UpdateSettings is PATCH /settings.
//
// Every field crosses as a pointer, on both sides, and that is the load
// bearing part rather than a style. Zero is a MEANING in three of the four
// capacity fields ("no cap", "no warning line", "no critical line") and
// --protect-last-known-good's zero value is already true, so a mapping
// that read values instead of pointers would turn "leave this alone" into
// "set it to zero" and "turn FR-19 protection off" into "leave it on",
// both while reporting success. The CLI goes to the trouble of telling
// those apart through fs.Visit; this is where that would be thrown away.
func (r *engineRoute) UpdateSettings(ctx context.Context, req service.UpdateSettingsRequest) (service.Settings, error) {
	body := apicontract.UpdateSettingsRequest{
		AcknowledgeMediumDisclosure: req.AcknowledgeMediumDisclosure,
	}
	if req.Retention != nil {
		retention := apicontract.UpdateRetentionSettings{
			Timezone:             req.Retention.Timezone,
			WeekStartsOn:         req.Retention.WeekStartsOn,
			ProtectLastKnownGood: req.Retention.ProtectLastKnownGood,
		}
		// Tiers used to be nil from this CLI, which did not expose a
		// whole-chain replacement, and was mapped anyway so that the one
		// type doing the translating would not have a hole in it the day
		// something else filled that field in. That day is #595:
		// `settings patch --policy-file` fills it, and this line is why
		// the routed half of it needed nothing.
		for _, t := range req.Retention.Tiers {
			retention.Tiers = append(retention.Tiers, retentionTierToWire(t))
		}
		body.Retention = &retention
	}
	if req.Capacity != nil {
		body.Capacity = &apicontract.UpdateCapacitySettings{
			CapBytes:          req.Capacity.CapBytes,
			WarningFreeBytes:  req.Capacity.WarningFreeBytes,
			CriticalFreeBytes: req.Capacity.CriticalFreeBytes,
			SafetyMarginBytes: req.Capacity.SafetyMarginBytes,
		}
	}

	resp, err := r.client.UpdateSettings(ctx, body)
	if err != nil {
		return service.Settings{}, err
	}
	return settingsFromWire(resp), nil
}

// settingsFromWire turns the engine's answer back into the shape
// printSettings already renders, so the two routes cannot grow two
// renderings of one policy.
//
// SettingsResponse's schema block is deliberately dropped: service.Settings
// carries no equivalent, nothing this command prints reads it, and
// inventing a field to hold it would be this adapter deciding what
// core/service's type is for.
func settingsFromWire(s apicontract.SettingsResponse) service.Settings {
	out := service.Settings{
		Retention: service.RetentionSettings{
			Timezone:             s.Retention.Timezone,
			WeekStartsOn:         s.Retention.WeekStartsOn,
			ProtectLastKnownGood: s.Retention.ProtectLastKnownGood,
		},
		Capacity: service.CapacitySettings{
			CapBytes:             s.Capacity.CapBytes,
			WarningFreeBytes:     s.Capacity.WarningFreeBytes,
			CriticalFreeBytes:    s.Capacity.CriticalFreeBytes,
			SafetyMarginBytes:    s.Capacity.SafetyMarginBytes,
			BackupRoot:           s.Capacity.BackupRoot,
			BackupRootConfigured: s.Capacity.BackupRootConfigured,
		},
	}
	for _, t := range s.Retention.Tiers {
		out.Retention.Tiers = append(out.Retention.Tiers, service.RetentionTier{
			Name:        t.Name,
			Granularity: t.Granularity,
			PeriodDays:  t.PeriodDays,
			Keep:        t.Keep,
			WindowUnit:  t.WindowUnit,
			Medium:      t.Medium,
		})
	}
	for _, m := range s.Mediums {
		out.Mediums = append(out.Mediums, storageMediumFromWire(m))
	}
	return out
}

func retentionTierToWire(t service.RetentionTier) apicontract.RetentionTier {
	return apicontract.RetentionTier{
		Name:        t.Name,
		Granularity: t.Granularity,
		PeriodDays:  t.PeriodDays,
		Keep:        t.Keep,
		WindowUnit:  t.WindowUnit,
		Medium:      t.Medium,
	}
}

// The storage-destination half of this route (G2.2, issue #594).
//
// It exists for the reason settingsRoute exists: `medium add`, `edit` and
// `remove` write configuration, and a configuration write left in the file
// beside a running engine is a change that process would never read
// (#538/#543). Without these, those three verbs would be permanently
// refused next to a live deployment, which is the position `settings
// patch` was in before #543.
//
// The import is the one that carries a secret, and it is the reason the
// CLI has no flag that takes one. The material is read from this
// process's stdin and put straight into a request body over the session
// this client already holds: never an argument, so never in the process
// table and never in shell history, and the response is an id, so nothing
// printed afterwards has anything to redact.

func (r *engineRoute) ImportStorageCredentials(ctx context.Context, accessKeyID, secretAccessKey, sessionToken string) (service.MediumCredentialRef, error) {
	resp, err := r.client.ImportStorageCredentials(ctx, apicontract.ImportStorageCredentialsRequest{
		AccessKeyID:     accessKeyID,
		SecretAccessKey: secretAccessKey,
		SessionToken:    sessionToken,
	})
	if err != nil {
		return service.MediumCredentialRef{}, err
	}
	// File is deliberately left empty. The engine wrote that path on its
	// own host, which may not be this one, and the API does not report it
	// for exactly that reason: an id is the whole of what a caller may
	// hold.
	return service.MediumCredentialRef{ID: resp.ID}, nil
}

func (r *engineRoute) ListStorageMediums(ctx context.Context) ([]service.StorageMediumSummary, error) {
	resp, err := r.client.ListStorageMediums(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]service.StorageMediumSummary, 0, len(resp.Mediums))
	for _, m := range resp.Mediums {
		out = append(out, storageMediumFromWire(m))
	}
	return out, nil
}

func (r *engineRoute) GetStorageMedium(ctx context.Context, id string) (service.StorageMediumSummary, error) {
	resp, err := r.client.GetStorageMedium(ctx, id)
	if err != nil {
		return service.StorageMediumSummary{}, err
	}
	return storageMediumFromWire(resp), nil
}

func (r *engineRoute) StorageMediumUsage(ctx context.Context, id string) (service.StorageMediumUsage, error) {
	resp, err := r.client.StorageMediumUsage(ctx, id)
	if err != nil {
		return service.StorageMediumUsage{}, err
	}
	out := service.StorageMediumUsage{Medium: resp.Medium, Placements: resp.Placements}
	for _, s := range resp.BackupSets {
		out.BackupSets = append(out.BackupSets, service.StorageMediumUsageBySet{
			Set: s.Set, Placements: s.Placements, OnlyCopyHere: s.OnlyCopyHere,
		})
	}
	return out, nil
}

func (r *engineRoute) PreflightStorageMediumCandidate(ctx context.Context, spec service.StorageMediumSpec) (service.MediumPreflight, error) {
	resp, err := r.client.PreflightStorageMediumCandidate(ctx, storageMediumToWire(spec))
	if err != nil {
		return service.MediumPreflight{}, err
	}
	return mediumPreflightFromWire(resp), nil
}

func (r *engineRoute) CreateStorageMedium(ctx context.Context, spec service.StorageMediumSpec) (service.StorageMediumSummary, error) {
	resp, err := r.client.CreateStorageMedium(ctx, storageMediumToWire(spec))
	if err != nil {
		return service.StorageMediumSummary{}, err
	}
	return storageMediumFromWire(resp), nil
}

func (r *engineRoute) UpdateStorageMedium(ctx context.Context, spec service.StorageMediumSpec) (service.StorageMediumSummary, error) {
	resp, err := r.client.UpdateStorageMedium(ctx, spec.ID, storageMediumToWire(spec))
	if err != nil {
		return service.StorageMediumSummary{}, err
	}
	return storageMediumFromWire(resp), nil
}

func (r *engineRoute) RemoveStorageMedium(ctx context.Context, id string) error {
	return r.client.RemoveStorageMedium(ctx, id)
}

// storageMediumFromWire is the one place the engine's answer becomes this
// binary's shape, shared by the settings read and by every medium verb, so
// the two cannot come to disagree about a destination.
func storageMediumFromWire(m apicontract.StorageMediumSummary) service.StorageMediumSummary {
	return service.StorageMediumSummary{
		ID:                  m.ID,
		Type:                m.Type,
		Bucket:              m.Bucket,
		Region:              m.Region,
		Endpoint:            m.Endpoint,
		Prefix:              m.Prefix,
		StorageClass:        m.StorageClass,
		UploadVerification:  m.UploadVerification,
		ReadsRequireRestore: m.ReadsRequireRestore,
	}
}

// storageMediumToWire is the write direction. The credentials block is
// sent as the caller spelled it, EMPTY INCLUDED: on an edit the engine
// reads an unnamed credential as "keep the one already configured", and a
// mapping that invented a value here would rotate a credential nobody
// asked to rotate.
func storageMediumToWire(spec service.StorageMediumSpec) apicontract.StorageMediumRequest {
	return apicontract.StorageMediumRequest{
		ID:                 spec.ID,
		Type:               spec.Type,
		Region:             spec.Region,
		Endpoint:           spec.Endpoint,
		Bucket:             spec.Bucket,
		Prefix:             spec.Prefix,
		StorageClass:       spec.StorageClass,
		UploadVerification: spec.UploadVerification,
		Credentials: apicontract.StorageMediumCredentialsReference{
			CredentialsID: spec.Credentials.ID,
			File:          spec.Credentials.File,
			Env:           spec.Credentials.Env,
			Command:       spec.Credentials.Command,
		},
	}
}

// mediumPreflightFromWire carries every check through, skipped ones
// included. A route that dropped them would print a shorter list on a
// failure than on a success, which is the one moment the full list matters
// most.
func mediumPreflightFromWire(r apicontract.MediumPreflightResponse) service.MediumPreflight {
	out := service.MediumPreflight{Medium: r.Medium, OK: r.OK, Checks: make([]service.MediumPreflightCheck, 0, len(r.Checks))}
	for _, c := range r.Checks {
		out.Checks = append(out.Checks, service.MediumPreflightCheck{
			Step: c.Step, Outcome: c.Outcome, Category: c.Category, Detail: c.Detail,
		})
	}
	return out
}
