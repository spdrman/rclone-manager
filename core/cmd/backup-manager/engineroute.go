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
		out.Mediums = append(out.Mediums, service.StorageMediumSummary{
			ID:                  m.ID,
			Type:                m.Type,
			Bucket:              m.Bucket,
			Region:              m.Region,
			StorageClass:        m.StorageClass,
			ReadsRequireRestore: m.ReadsRequireRestore,
		})
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
