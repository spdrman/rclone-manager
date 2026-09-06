package apiclient

import (
	"context"

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
// - the mutating backup-set verbs, and the reads `sources` and `status`
// are built on - plus the session verbs. It is deliberately not all
// forty-six operations: an untested wrapper around an endpoint no command
// calls is a claim that this client works against it, and nothing here has
// watched that claim fail. Adding one is three lines and a contract id.

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

// SetBackupSetEnabled is POST /backup-sets/{source}/{set}/enabled.
func (c *Client) SetBackupSetEnabled(ctx context.Context, source, set string, req apicontract.SetEnabledRequest) (apicontract.BackupSet, error) {
	var out apicontract.BackupSet
	err := c.call(ctx, "setBackupSetEnabled", []string{source, set}, req, &out)
	return out, err
}

// SetBackupSetReadOnly is POST /backup-sets/{source}/{set}/read-only.
func (c *Client) SetBackupSetReadOnly(ctx context.Context, source, set string, req apicontract.SetReadOnlyRequest) (apicontract.BackupSet, error) {
	var out apicontract.BackupSet
	err := c.call(ctx, "setBackupSetReadOnly", []string{source, set}, req, &out)
	return out, err
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
