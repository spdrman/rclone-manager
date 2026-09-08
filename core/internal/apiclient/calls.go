package apiclient

import (
	"context"
	"net/url"
	"strconv"

	"github.com/spdrman/rclone-manager/core/apicontract"
)

// The typed calls.
//
// Each one names a contract operation id and hands over its path
// parameters in the order the contract declares them; none of them builds
// a URL, and none of them declares a request or response type. That is the
// point: the path, the method, whether a session is required, whether the
// double-submit token is required, which status counts as success and
// which error codes are legal all come from core/apicontract, so an
// operation that changes in api/v1/openapi.json changes here by
// regeneration rather than by somebody noticing.
//
// The set below is the surface the commands issue #543 and #544 route need
// - the mutating backup-set verbs, the two steps a create takes before
// them, and the five operations the reads `sources`, `status`, `artifacts`
// and the retention preview put their questions to - plus the
// session verbs. It is deliberately not all forty-six operations: an
// untested wrapper around an endpoint no command calls is a claim that
// this client works against it, and nothing here has watched that claim
// fail. Adding one is three lines and a contract id.
//
// setBackupSetEnabled and setBackupSetReadOnly used to be here and are
// gone, under that same rule rather than in spite of it. No command calls
// either: this CLI has no enable/disable verb at all, and --read-only is a
// field of a create rather than a verb of its own, so both were wrappers
// nothing had ever driven against the routes they name. #543 is where they
// would have acquired a caller and did not.

// ListBackupSets is GET /backup-sets: the configuration the ENGINE holds,
// which is the whole reason a CLI would ask over HTTP rather than read the
// file itself.
func (c *Client) ListBackupSets(ctx context.Context) (apicontract.ListBackupSetsResponse, error) {
	var out apicontract.ListBackupSetsResponse
	err := c.call(ctx, "listBackupSets", nil, nil, &out)
	return out, err
}

// GetBackupSet is GET /backup-sets/{source}/{set}.
//
// Two arguments rather than one composite id, because that is what the
// contract publishes and what the engine routes. It used to be one, back
// when the document spelled this path "/backup-sets/{id}": fillPath
// escapes a parameter, as it must, so "production/postgres" went out as
// "production%2Fpostgres" and every backup set that existed came back
// BACKUP_SET_NOT_FOUND (PR #546 review).
func (c *Client) GetBackupSet(ctx context.Context, source, set string) (apicontract.BackupSet, error) {
	var out apicontract.BackupSet
	err := c.call(ctx, "getBackupSet", []string{source, set}, nil, &out)
	return out, err
}

// CreateBackupSet is POST /backup-sets, the operation whose CLI twin
// wrote past a running engine in issue #535.
func (c *Client) CreateBackupSet(ctx context.Context, req apicontract.CreateBackupSetRequest) (apicontract.CreateBackupSetResponse, error) {
	var out apicontract.CreateBackupSetResponse
	err := c.call(ctx, "createBackupSet", nil, req, &out)
	return out, err
}

// ImportSSHKey is POST /ssh-keys: the private key this deployment will
// use to reach a source, handed over once so the engine can keep its own
// copy.
//
// It is here because `backup-set create --ssh-key-file` has to import
// before it can name a key id, and importing into whatever filesystem the
// CLI happens to be running on is importing into the wrong place when the
// key store belongs to the process being routed to. The request carries
// key material, which is the only body in this package that does; it is
// built by the caller from a file it read, sent once, and held nowhere
// else. Nothing here logs it, and the response deliberately carries a
// reference and a fingerprint rather than the key.
func (c *Client) ImportSSHKey(ctx context.Context, req apicontract.ImportSSHKeyRequest) (apicontract.ImportSSHKeyResponse, error) {
	var out apicontract.ImportSSHKeyResponse
	err := c.call(ctx, "importSSHKey", nil, req, &out)
	return out, err
}

// ProbeHostKey is POST /ssh/host-key-probe: open a connection to a source
// from where the ENGINE is, and report the host key that answered.
//
// Which end probes is the whole point. `--trust-host-key` is trust on
// first use, and the machine that has to be able to reach the source is
// the one that will be pulling backups off it. A CLI that probed from its
// own host would trust a key seen from somewhere the backups never travel,
// and would fail outright wherever the source is only reachable from the
// engine's network.
func (c *Client) ProbeHostKey(ctx context.Context, req apicontract.HostKeyProbeRequest) (apicontract.HostKeyProbeResponse, error) {
	var out apicontract.HostKeyProbeResponse
	err := c.call(ctx, "probeHostKey", nil, req, &out)
	return out, err
}

// UpdateBackupSet is PATCH /backup-sets/{source}/{set}.
func (c *Client) UpdateBackupSet(ctx context.Context, source, set string, req apicontract.UpdateBackupSetRequest) (apicontract.BackupSet, error) {
	var out apicontract.BackupSet
	err := c.call(ctx, "updateBackupSet", []string{source, set}, req, &out)
	return out, err
}

// RemoveBackupSet is DELETE /backup-sets/{source}/{set}.
func (c *Client) RemoveBackupSet(ctx context.Context, source, set string) error {
	return c.call(ctx, "removeBackupSet", []string{source, set}, nil, nil)
}

// GetBackupSetRetention is GET /backup-sets/{source}/{set}/retention.
func (c *Client) GetBackupSetRetention(ctx context.Context, source, set string) (apicontract.BackupSetRetention, error) {
	var out apicontract.BackupSetRetention
	err := c.call(ctx, "getBackupSetRetention", []string{source, set}, nil, &out)
	return out, err
}

// SetBackupSetRetention is PUT /backup-sets/{source}/{set}/retention.
func (c *Client) SetBackupSetRetention(ctx context.Context, source, set string, req apicontract.RetentionOverride) (apicontract.BackupSetRetention, error) {
	var out apicontract.BackupSetRetention
	err := c.call(ctx, "setBackupSetRetention", []string{source, set}, req, &out)
	return out, err
}

// ClearBackupSetRetention is DELETE /backup-sets/{source}/{set}/retention,
// which answers with the policy the set falls back to rather than with
// nothing.
func (c *Client) ClearBackupSetRetention(ctx context.Context, source, set string) (apicontract.BackupSetRetention, error) {
	var out apicontract.BackupSetRetention
	err := c.call(ctx, "clearBackupSetRetention", []string{source, set}, nil, &out)
	return out, err
}

// GetSettings is GET /settings: the retention and capacity policy the
// ENGINE is deciding with, resolved, which is a different fact from what
// this host's config.yaml says and the whole reason to ask over HTTP.
func (c *Client) GetSettings(ctx context.Context) (apicontract.SettingsResponse, error) {
	var out apicontract.SettingsResponse
	err := c.call(ctx, "getSettings", nil, nil, &out)
	return out, err
}

// UpdateSettings is PATCH /settings, the other configuration write a
// terminal can make. It is here rather than left for later because
// `settings patch` is refused beside a running engine (#538) and this is
// the only thing that can replace that refusal with a change that lands.
func (c *Client) UpdateSettings(ctx context.Context, req apicontract.UpdateSettingsRequest) (apicontract.SettingsResponse, error) {
	var out apicontract.SettingsResponse
	err := c.call(ctx, "updateSettings", nil, req, &out)
	return out, err
}

// SystemHealth is GET /system/health, half of what `status` reports.
func (c *Client) SystemHealth(ctx context.Context) (apicontract.HealthResponse, error) {
	var out apicontract.HealthResponse
	err := c.call(ctx, "getSystemHealth", nil, nil, &out)
	return out, err
}

// SystemVersion is GET /system/version. It carries the engine's own
// config_revision, which is what lets a command say WHICH configuration
// the answer came from rather than only what it said.
func (c *Client) SystemVersion(ctx context.Context) (apicontract.VersionResponse, error) {
	var out apicontract.VersionResponse
	err := c.call(ctx, "getSystemVersion", nil, nil, &out)
	return out, err
}

// StorageStatus is GET /system/storage, the other half of `status`.
func (c *Client) StorageStatus(ctx context.Context) (apicontract.ListStorageStatusResponse, error) {
	var out apicontract.ListStorageStatusResponse
	err := c.call(ctx, "listStorageStatus", nil, nil, &out)
	return out, err
}

// ListArtifacts is GET /backups: every artifact the ENGINE's journal holds
// at the moment it is asked, optionally narrowed to one backup set.
//
// setID is a "source/set" id and goes in the QUERY rather than the path,
// which is why this is the one call in this package that sends one at all.
// An empty setID sends no query, and that is not the same request as
// ?setId=: the contract refuses an id naming no configured backup set
// rather than answering it with an empty list, so an empty filter sent as
// a filter would turn "show me everything" into a 404.
//
// The unfiltered listing includes the artifacts of backup sets whose
// configuration was removed (issue #391), and every artifact carries
// retention_policy so a caller can tell those apart (issue #523).
func (c *Client) ListArtifacts(ctx context.Context, setID string) (apicontract.ListArtifactsResponse, error) {
	var query url.Values
	if setID != "" {
		query = url.Values{"setId": []string{setID}}
	}
	var out apicontract.ListArtifactsResponse
	err := c.callQuery(ctx, "listArtifacts", nil, query, nil, &out)
	return out, err
}

// GetArtifact is GET /backups/{source}/{set}/{name}.
//
// Three arguments rather than one composite id, for GetBackupSet's reason
// one level deeper: the contract used to spell this "/backups/{id}", and
// an artifact id is three segments, so fillPath escaped two slashes into
// one unroutable parameter and this operation could not be called at all
// (PR #546 review).
func (c *Client) GetArtifact(ctx context.Context, source, set, name string) (apicontract.Artifact, error) {
	var out apicontract.Artifact
	err := c.call(ctx, "getArtifact", []string{source, set, name}, nil, &out)
	return out, err
}

// PreviewRetention is GET /backup-sets/{source}/{set}/retention/preview:
// the plan the ENGINE would apply, derived from the configuration that
// engine holds rather than from the file on disk.
//
// It issues a plan_id and deletes nothing. FR-20's deletion runs through
// applyRetention, which refuses unless the plan it re-derives still
// fingerprints as the one that id was issued for, and this package
// deliberately has no method for that: `backup-manager retention` is a
// preview in both its modes (retention.go's own doc) and a CLI apply would
// be a second authorisation path beside the one an administrator reviews.
func (c *Client) PreviewRetention(ctx context.Context, source, set string) (apicontract.RetentionPlan, error) {
	var out apicontract.RetentionPlan
	err := c.call(ctx, "previewRetention", []string{source, set}, nil, &out)
	return out, err
}

// ListActivity is GET /activity: the deployment-wide lifecycle feed, newest
// first.
//
// limit is advisory in both directions and matches the route's own
// handling: zero or less sends no query at all and takes the engine's
// default, and a number above the engine's maximum is clamped there rather
// than refused. A caller asking for a feed gets a feed.
//
// This is the one read in this package whose answer is RENDERED rather
// than compared. readmode.go's doc explains why the other four are
// comparisons: each of those commands prints something the contract cannot
// express, so re-rendering from the wire would print less than the direct
// route does. This feed has no such gap. Every column `activity` shows is
// a field of ActivityEvent, so beside a live engine the honest thing is to
// print the engine's own answer, and a set comparison would be worse than
// useless here anyway: the log grows while the two reads happen, so two
// truthful answers taken a round trip apart legitimately differ at the
// newest end.
func (c *Client) ListActivity(ctx context.Context, limit int) (apicontract.ListActivityResponse, error) {
	var query url.Values
	if limit > 0 {
		query = url.Values{"limit": []string{strconv.Itoa(limit)}}
	}
	var out apicontract.ListActivityResponse
	err := c.callQuery(ctx, "listActivity", nil, query, nil, &out)
	return out, err
}
