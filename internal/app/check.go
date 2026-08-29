package app

import (
	"context"
	"fmt"

	"github.com/spdrman/rclone-manager/internal/config"
	"github.com/spdrman/rclone-manager/internal/state"
)

// Check is the `backup-manager check` use case: a pre-flight answer to "can
// this deployment actually start", checked before anything is asked to
// process a single artifact.
//
// It is a package-level function, not a Service method, on purpose: a
// Service cannot exist yet at this point, since building one needs an
// already-validated Config and an already-open Journal, exactly the two
// things Check exists to produce (or fail loudly on producing).
//
// Check does two things, in order:
//
//  1. config.LoadAndValidate(configPath): parses and validates FR-5's
//     configuration file. Every problem config.Validate can find (a
//     missing field, a duplicate backup set id, an invalid timezone, ...)
//     is reported here, in one pass, rather than surfacing one at a time
//     the first time something downstream happens to touch the field.
//  2. state.Open against the configured database path: opens (creating if
//     necessary) the FR-9 lifecycle journal and runs its migrations. This
//     is a real, meaningful check, not a formality: it proves the
//     configured state.database path is writable, that SQLite's WAL mode
//     and synchronous=FULL pragmas apply cleanly, and that this binary's
//     embedded migrations either already match what is on disk or apply
//     to it without conflict (see internal/state.Open's own doc for
//     ErrUnknownSchemaVersion and ErrSchemaDrift, both of which Check
//     surfaces exactly as state.Open reports them).
//
// Check does not contact any configured remote: proving connectivity to a
// source is a materially different, slower and credential-dependent check
// than "is this config and this database usable", and conflating the two
// would make Check's failure mode ambiguous (a bad password and a typo in
// local_path would look identical). `backup-manager reconcile` and
// `backup-manager fetch` are what exercise real connectivity.
//
// The returned *config.Config is the same up-to-date result LoadAndValidate
// produced, so a caller (cmd/backup-manager's `check` command) can print a
// summary of what was validated without loading the file a second time.
func Check(ctx context.Context, configPath string) (*config.Config, error) {
	cfg, err := config.LoadAndValidate(configPath)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	j, err := state.Open(ctx, cfg.State.Database)
	if err != nil {
		return cfg, fmt.Errorf("state: %w", err)
	}
	if err := j.Close(); err != nil {
		return cfg, fmt.Errorf("state: closing %s: %w", cfg.State.Database, err)
	}

	return cfg, nil
}
