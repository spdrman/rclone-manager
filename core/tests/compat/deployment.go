package compat

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// fixedNow is the instant every verdict cell is decided at.
//
// It is the same instant internal/retention's own golden baseline uses, on
// purpose: the two are answering the same question from opposite sides
// (that one pins DecideKeep against in-memory fixtures, this one pins it
// against records that came back out of a migrated database), and a shared
// clock means a difference between them is a real difference and not a
// calendar artifact.
var fixedNow = time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

// seedSpec is one artifact to plant, with everything about it fixed.
type seedSpec struct {
	name string

	// state is the lifecycle state to leave the artifact in. The journal
	// deliberately does not police transition legality (see its package
	// doc), so this seeds each artifact in two writes, DISCOVERED and
	// then the target, exactly as core/service's own retention tests do.
	state lifecycle.State

	discoveredAt time.Time

	// content is written to the backup set's local directory, so FR-20's
	// filesystem checks have a real file to canonicalize and refuse or
	// accept. An empty content means the file is deliberately NOT
	// written: that is the prune REFUSE case, and it is here because a
	// prune cell with no refusal in it certifies only the happy path.
	content string

	// retentionTier, when set, is recorded on the row the way a previous
	// retention pass would have recorded it. At least one row has to
	// carry a non-empty one, because the compatibility violation the
	// spec's section 4 table names is a backfill that REWRITES
	// retention_tier, and a column that is empty on every row cannot
	// show the difference between "rewritten" and "never set".
	retentionTier string

	validationPassed *bool
}

// theDeployment is the medium-free deployment every state-backed cell is
// captured from: one backup set, ten artifacts, chosen to reach every
// verdict this product can produce.
//
// Naming the coverage rather than assuming it: three GFS tiers, an
// artifact outside every window, a sibling collision (two artifacts on the
// same instant in the same bucket, one of which must lose), the
// quarantined-newest trap, three non-COMPLETE lifecycle states that
// retention must decline to act on, an artifact whose local file is
// missing so prune has to REFUSE rather than DELETE, and one row carrying
// a recorded retention tier.
//
// The three siblings are why the missing file sits on one of them rather
// than on an artifact of its own: every artifact outside a bucket
// collision is kept by SOME tier here (the annual tier reaches back seven
// years), so a lone old artifact would have produced a KEEP verdict and
// prune would never have reached its safety checks at all. Losing a
// sibling tie-break is the only way to be a delete candidate in this
// chain, so two of the three lose, one of those two has no file on disk,
// and the prune cell therefore carries a real DELETE and a real REFUSE
// instead of nine KEEPs.
func theDeployment() []seedSpec {
	passed := true
	failed := false
	return []seedSpec{
		{name: "too-old-everything.dump", state: lifecycle.Complete, discoveredAt: date(2025, 1, 1), content: "older than every window", validationPassed: &passed},
		{name: "monthly-only.dump", state: lifecycle.Complete, discoveredAt: date(2026, 3, 15), content: "kept by the monthly tier alone", retentionTier: "MONTHLY", validationPassed: &passed},
		{name: "sibling-a.dump", state: lifecycle.Complete, discoveredAt: date(2026, 6, 10), content: "", validationPassed: &passed},
		{name: "sibling-b.dump", state: lifecycle.Complete, discoveredAt: date(2026, 6, 10), content: "sibling b", validationPassed: &passed},
		{name: "sibling-c.dump", state: lifecycle.Complete, discoveredAt: date(2026, 6, 10), content: "sibling c", validationPassed: &passed},
		{name: "week-old-in-weekly.dump", state: lifecycle.Complete, discoveredAt: date(2026, 8, 15), content: "kept by the weekly tier", validationPassed: &passed},
		{name: "recent-daily.dump", state: lifecycle.Complete, discoveredAt: date(2026, 8, 28), content: "the newest eligible artifact", retentionTier: "DAILY", validationPassed: &passed},
		{name: "quarantined-newest.dump", state: lifecycle.Quarantined, discoveredAt: date(2026, 8, 29), content: "newest, and not eligible for anything", validationPassed: &failed},
		{name: "still-committed.dump", state: lifecycle.Committed, discoveredAt: date(2026, 8, 27), content: "owes FR-15 a remote delete"},
		{name: "remote-delete-pending.dump", state: lifecycle.RemoteDeletePending, discoveredAt: date(2026, 8, 26), content: "mid remote delete"},
	}
}

func date(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 9, 0, 0, 0, time.UTC)
}

// backupSetFor builds the config.BackupSet the seeded deployment describes,
// resolved through config.Validate so its retention is the real resolved
// policy rather than a hand-built one.
func backupSetFor(root, configYAML string) (config.BackupSet, *config.Config, error) {
	cfgPath := filepath.Join(root, "config.yaml")
	if err := writeConfig(cfgPath, configYAML); err != nil {
		return config.BackupSet{}, nil, err
	}
	cfg, err := config.LoadAndValidate(cfgPath)
	if err != nil {
		return config.BackupSet{}, nil, err
	}
	return cfg.Sources[0].BackupSets[0], cfg, nil
}

// deploymentConfigYAML is the medium-free config the whole gate is about.
//
// The chain is spelled out rather than left to the three legacy scalars so
// the corpus pins a real multi-tier chain, and no tier carries a medium
// key, because that is the deployment FR-35 promises nothing changes for.
func deploymentConfigYAML(root string) string {
	return fmt.Sprintf(`poll_interval: 15m

state:
  database: %s/state.db

sources:
  - id: production
    backup_sets:
      - id: postgres-primary
        remote:
          type: local
        remote_path: %s/exports
        local_path: %s/backups
        completion:
          strategy: stable
          stable_for: 10m
        stale_after: 30h

retention:
  timezone: UTC
  week_starts_on: monday
  protect_last_known_good: true
  tiers:
    - name: daily
      granularity: day
      keep: 7
    - name: weekly
      granularity: week
      keep: 3
      window_unit: month
    - name: monthly
      granularity: month
      keep: 12
    - name: annual
      granularity: year
      keep: 7
`, root, root, root)
}

// seedDeployment lays down the directories, writes the artifact files and
// records every artifact through the public journal API, applying every
// migration this binary carries on the way in.
func seedDeployment(ctx context.Context, root, configYAML string, specs []seedSpec) (config.BackupSet, *config.Config, error) {
	for _, dir := range []string{filepath.Join(root, "backups"), filepath.Join(root, "exports")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return config.BackupSet{}, nil, err
		}
	}

	bs, cfg, err := backupSetFor(root, configYAML)
	if err != nil {
		return config.BackupSet{}, nil, err
	}

	journal, err := state.Open(ctx, filepath.Join(root, "state.db"))
	if err != nil {
		return config.BackupSet{}, nil, err
	}
	defer journal.Close() //nolint:errcheck // read-back happens through a fresh handle

	for _, spec := range specs {
		artifact, err := model.NewArtifactID(bs.ID, spec.name)
		if err != nil {
			return config.BackupSet{}, nil, err
		}

		localPath := filepath.Join(bs.LocalPath, spec.name)
		size := int64(len(spec.content))
		hash := sha256.Sum256([]byte(spec.content))
		if spec.content != "" {
			if err := os.WriteFile(localPath, []byte(spec.content), 0o644); err != nil {
				return config.BackupSet{}, nil, err
			}
		}

		mtime := spec.discoveredAt.Add(-time.Hour)
		if _, err := journal.RecordTransition(ctx, state.Transition{
			Artifact:   artifact,
			Key:        "compat-discover-" + spec.name,
			From:       "",
			To:         string(lifecycle.Discovered),
			OccurredAt: spec.discoveredAt,
			RemotePath: "/exports/" + spec.name,
			Remote: &state.RemoteIdentity{
				Size:      &size,
				ModTime:   &mtime,
				Hash:      hex.EncodeToString(hash[:]),
				HashAlg:   "sha256",
				BackendID: "local",
			},
		}); err != nil {
			return config.BackupSet{}, nil, fmt.Errorf("seeding %s at DISCOVERED: %w", spec.name, err)
		}

		lp := localPath
		t := state.Transition{
			Artifact:   artifact,
			Key:        "compat-settle-" + spec.name,
			From:       string(lifecycle.Discovered),
			To:         string(spec.state),
			OccurredAt: spec.discoveredAt.Add(time.Minute),
			LocalPath:  &lp,
			Transfer:   &state.TransferResult{BytesTransferred: size, Checksummed: true},
			Hashes:     &state.HashUpdate{Hash: hex.EncodeToString(hash[:]), Alg: "sha256"},
		}
		if spec.validationPassed != nil {
			t.Validation = &state.ValidationUpdate{
				Passed: *spec.validationPassed,
				Detail: validationDetailFor(*spec.validationPassed),
			}
		}
		if spec.retentionTier != "" {
			expires := spec.discoveredAt.AddDate(1, 0, 0)
			t.Retention = &state.RetentionUpdate{Tier: spec.retentionTier, ExpiresAt: &expires}
		}
		if spec.state == lifecycle.Complete {
			deleted := spec.discoveredAt.Add(2 * time.Minute)
			t.Deletion = &state.DeletionUpdate{DeletedAt: &deleted}
		}
		if _, err := journal.RecordTransition(ctx, t); err != nil {
			return config.BackupSet{}, nil, fmt.Errorf("seeding %s at %s: %w", spec.name, spec.state, err)
		}
	}

	return bs, cfg, nil
}

func validationDetailFor(passed bool) string {
	if passed {
		return "validator exited 0"
	}
	return "validator exited 1: refused"
}
