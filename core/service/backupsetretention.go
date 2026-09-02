// This file finishes issue #333. The config and engine slice landed with
// #362: a backup set can carry its own retention chain, "absent" and
// "explicitly set" stay apart in the parsed config, resolution runs
// through one seam, and a preview says which policy was in force. What
// was left was the part an operator can actually reach. Until now the
// only way to give one set its own chain was to open config.yaml in an
// editor on the NAS, which is the same gap issue #350 closed for the rest
// of a backup set's definition.
//
// # Three operations, one persist path
//
// Show, set and clear, exactly as the issue asks for. Set and clear are
// thin wrappers over UpdateBackupSet (backupsetupdate.go) rather than a
// second write path beside it, so a retention override is persisted by
// the same re-read / encode-before-Validate / atomic-write / hot-reload
// sequence every other field goes through, and #336's own rule -- any
// mutation of a retention policy has to be followed by Validate, or every
// set goes on deciding under the policy that was in force when it was
// last resolved -- holds here because that is literally the code that
// runs.
//
// The route into the config is a tri-state, not a pointer, because there
// are three things a request can say about a set's override and a
// *policy could only say two: leave it alone, install this one, remove
// it. Absent and null are the same value in JSON, so a pointer would
// make "clear" unspellable on the wire.
//
// # There is exactly one validation seam, and it is not here
//
// Nothing in this file validates a chain. A per-set policy is refused on
// exactly the same terms as a global one because it goes through
// config.Validate's own resolveBackupSetRetention, which is where the
// whole-chain rule and the calendar inheritance live. A second check here
// would be a second rule the day the first one changed, which is the
// thing the issue is most emphatic about.
//
// # An override is written as a tier chain, always
//
// BackupSetRetentionOverride carries Tiers and nothing else that names a
// chain. The three legacy daily_days/weekly_months/monthly_months scalars
// stay readable, because a hand-written file may use them and a read
// resolves either spelling through EffectiveTiers, but nothing on these
// three operations can WRITE them.
//
// That is what makes the sharpest failure this feature has unreachable
// from here rather than merely refused. A tiers list is a whole chain by
// construction; the scalars are sugar for one specific three-tier chain,
// so a request naming two of the three would be half a chain, and half a
// chain one level down resolves the missing half to the PRODUCT default
// rather than to the deployment's policy. A deployment retaining 90/24/60
// with one set asking for daily_days alone would resolve to 120/3/12,
// collapsing weekly from 24 months to 3 and monthly from 60 to 12,
// silently, which is deletion of data the operator believes is retained.
// The resolver refuses that; this shape means no caller can express it.
//
// # Show reports the RESOLVED policy, and says where it came from
//
// Not the raw override. The question an operator asks a `show` is "what
// is this set retained under", and the answer is the chain after
// inheritance has been worked out: an override that omits the timezone is
// reckoned in the deployment's, and reporting the raw block would leave
// the reader to redo that resolution in their head. IsOverride beside it
// is what "why is this artifact being deleted" needs, because a set
// override and a deployment policy that happen to agree produce an
// identical chain and want opposite advice ("edit the set" versus "edit
// the deployment").
package service

import (
	"context"
	"fmt"

	"github.com/spdrman/rclone-manager/core/internal/config"
)

// BackupSetRetentionOverride is one backup set's own retention policy, as
// a caller writes it.
//
// Tiers is required and is the whole chain, for the reason this file's
// doc gives at length. Everything else is optional and inherits from the
// deployment's resolved policy when omitted: the timezone and the week
// start decide how ANY chain is reckoned rather than what the chain says,
// and an override that omitted the timezone and got UTC instead of the
// deployment's own would silently move which day every restore point
// belongs to for most of the world.
type BackupSetRetentionOverride struct {
	// Tiers is the chain this backup set is retained under. An empty
	// chain is refused rather than read as "keep nothing": there is no
	// "keep nothing" spelling in this schema at all, and retention is
	// turned off by not running a retention pass.
	Tiers []RetentionTier

	// Timezone and WeekStartsOn are "" to inherit the deployment's.
	Timezone     string
	WeekStartsOn string

	// ProtectLastKnownGood is nil to inherit the deployment's posture.
	// An explicit false is written through as an explicit false, and
	// internal/retention calls that "a materially more dangerous
	// configuration"; as on the deployment-wide write path, showing an
	// operator what that means belongs in front of the human rather than
	// here, which cannot tell one who confirmed from one never asked.
	ProtectLastKnownGood *bool
}

// BackupSetRetention is what GetBackupSetRetention answers: what this set
// is retained under, and whether it says so itself.
type BackupSetRetention struct {
	BackupSetID string

	// IsOverride reports whether the set declares its own policy rather
	// than inheriting the deployment's. It reads whether the operator
	// wrote a block, never whether the two chains happen to match: a set
	// may legitimately declare a chain identical to the deployment's, and
	// "the operator wrote this here" is what a preview has to report.
	IsOverride bool

	// Policy is the fully-resolved chain in force for this set,
	// whichever of the two it came from.
	Policy RetentionSettings

	// DeploymentPolicy is the resolved deployment-wide policy, reported
	// alongside so a caller can show what clearing the override would go
	// back to without a second request, and so a UI can put the two side
	// by side. It equals Policy exactly when IsOverride is false.
	DeploymentPolicy RetentionSettings
}

// retentionOverrideChange is the tri-state UpdateBackupSetRequest carries
// for a set's retention override: nil leaves it alone, Clear removes it,
// and an Override installs one. See this file's doc for why a pointer to
// the policy alone could not express all three.
type retentionOverrideChange struct {
	Clear    bool
	Override BackupSetRetentionOverride
}

// GetBackupSetRetention is issue #333's "show": what this backup set is
// retained under right now, and whether that is its own policy or the
// deployment's.
//
// It reads this service's already-validated configuration rather than the
// file. Those can differ for a CLI invocation run against a file a
// separate running daemon loaded earlier, and the honest answer to "what
// is this set retained under" is the one the process answering would
// actually decide with.
func (b *BackupService) GetBackupSetRetention(_ context.Context, id string) (BackupSetRetention, error) {
	sourceName, setName, ok := splitBackupSetID(id)
	if !ok {
		return BackupSetRetention{}, wrapNotFound(id)
	}
	cfg := b.state.Load().inner.Config
	bs, found := findBackupSetIn(cfg, sourceName, setName)
	if !found {
		return BackupSetRetention{}, wrapNotFound(id)
	}
	return BackupSetRetention{
		BackupSetID:      id,
		IsOverride:       bs.RetentionIsOverride(),
		Policy:           toRetentionSettings(bs.Retention),
		DeploymentPolicy: toRetentionSettings(cfg.Retention),
	}, nil
}

// SetBackupSetRetention is issue #333's "set": give this backup set its
// own retention chain, whatever the rest of the deployment is retained
// under. Editing the deployment's policy afterwards does not move it.
//
// It replaces any override already there rather than merging with it,
// which is the same whole-policy rule the config schema follows: merging
// tier by tier across two chains produces a policy nobody wrote and
// nobody can predict from reading either half.
func (b *BackupService) SetBackupSetRetention(ctx context.Context, id string, override BackupSetRetentionOverride) (BackupSetRetention, error) {
	if len(override.Tiers) == 0 {
		// Refused here rather than left to the resolver, because the
		// resolver would see an override naming no chain at all and say
		// "name daily_days, weekly_months and monthly_months", which is
		// advice about a spelling this surface does not accept. The rule
		// is the same one; only the sentence is local.
		return BackupSetRetention{}, fmt.Errorf(
			"%w: a backup set's own retention policy has to name at least one tier; to go back to the deployment's policy, clear the override instead", ErrInvalidRequest)
	}
	if _, err := b.UpdateBackupSet(ctx, id, UpdateBackupSetRequest{
		Retention: &retentionOverrideChange{Override: override},
	}); err != nil {
		return BackupSetRetention{}, err
	}
	return b.GetBackupSetRetention(ctx, id)
}

// ClearBackupSetRetention is issue #333's "clear": this set goes back to
// being retained under the deployment's policy, with no residue of the
// chain it used to declare.
//
// Clearing an override on a set that has none is a success, deliberately:
// the state the caller asked for is the state that holds, and a client
// that has to check first before it can safely ask is a client that will
// race. It also means clear does not rewrite config.yaml for a set it
// would not change, which matters because a no-op write still moves the
// configuration revision and invalidates every outstanding retention
// preview. An unknown set is still refused, because that is a different
// answer.
//
// # The one window this has, said rather than left to be found
//
// The "is there anything to clear" read is not taken under the same lock
// the write is: UpdateBackupSet takes configMu itself and re-reads the
// file inside it, so this method cannot hold it across both. A set that
// gains an override between the read and the return therefore gets a
// clear that does nothing and an answer saying it inherits.
//
// That is bounded rather than silent: nothing wrong is written, the next
// read shows the override still there, and the losing caller is a second
// operator writing the same set's retention policy at the same
// millisecond as the first. Closing it properly means the no-op decision
// moving inside UpdateBackupSet's own lock, which is a change to the
// update path's contract for every field rather than for this one, and
// is not worth making from here. The fresh read below is what keeps the
// returned value a real one rather than the pre-write snapshot.
func (b *BackupService) ClearBackupSetRetention(ctx context.Context, id string) (BackupSetRetention, error) {
	current, err := b.GetBackupSetRetention(ctx, id)
	if err != nil {
		return BackupSetRetention{}, err
	}
	if !current.IsOverride {
		return b.GetBackupSetRetention(ctx, id)
	}
	if _, err := b.UpdateBackupSet(ctx, id, UpdateBackupSetRequest{
		Retention: &retentionOverrideChange{Clear: true},
	}); err != nil {
		return BackupSetRetention{}, err
	}
	return b.GetBackupSetRetention(ctx, id)
}

// findBackupSetIn is findBackupSet (backupsets.go) over a config the
// caller already holds rather than one it re-reads, plus a found flag so
// "no such set" is not confused with a set whose every field is zero.
func findBackupSetIn(cfg *config.Config, sourceName, setName string) (config.BackupSet, bool) {
	for _, src := range cfg.Sources {
		if src.Name != sourceName {
			continue
		}
		for _, bs := range src.BackupSets {
			if bs.Name == setName {
				return bs, true
			}
		}
	}
	return config.BackupSet{}, false
}

// toConfigRetentionOverride builds the config-level block an override is
// persisted as. It writes a tiers chain and never the legacy scalars, so
// a set this surface configures always reads back as the chain it was
// given, and it copies rather than aliases the caller's slice, since what
// it returns is written into the configuration this process is about to
// persist and resolve.
func toConfigRetentionOverride(o BackupSetRetentionOverride) config.Retention {
	out := config.Retention{
		Timezone:     o.Timezone,
		WeekStartsOn: o.WeekStartsOn,
		Tiers:        make([]config.RetentionTier, 0, len(o.Tiers)),
	}
	if o.ProtectLastKnownGood != nil {
		protect := *o.ProtectLastKnownGood
		out.ProtectLastKnownGood = &protect
	}
	for _, t := range o.Tiers {
		out.Tiers = append(out.Tiers, config.RetentionTier{
			Name:        t.Name,
			Granularity: t.Granularity,
			PeriodDays:  t.PeriodDays,
			Keep:        t.Keep,
			WindowUnit:  t.WindowUnit,
			Medium:      t.Medium,
		})
	}
	return out
}

// DescribeRetentionPolicy renders a resolved chain as one line, for a CLI
// or a log that has to fit it beside a set id. It names every tier in
// chain order with its own keep and unit, because that order is what
// decides where a kept artifact lives once EPIC E's mediums act on it
// (config.Retention.Tiers' own doc), so a summary that reordered or
// elided tiers would stop being the chain.
func DescribeRetentionPolicy(p RetentionSettings) string {
	if len(p.Tiers) == 0 {
		// A resolved policy always has a chain. A caller holding an
		// unresolved one should see that said, rather than an empty pair
		// of brackets that reads like "keeps nothing".
		return "(no chain resolved)"
	}
	out := "tiers=["
	for i, t := range p.Tiers {
		if i > 0 {
			out += " "
		}
		unit := t.WindowUnit
		if unit == "" {
			unit = t.Granularity
		}
		out += t.Name + "/" + fmt.Sprint(t.Keep) + unitSuffix(unit, t.PeriodDays)
		if t.Medium != "" {
			out += "@" + t.Medium
		}
	}
	out += "]"
	if p.Timezone != "" {
		out += " timezone=" + p.Timezone
	}
	if p.WeekStartsOn != "" {
		out += " week_starts_on=" + p.WeekStartsOn
	}
	if !p.ProtectLastKnownGood {
		// Named only when it is OFF. On is the default and the safe
		// posture, so saying so on every line would train an operator to
		// stop reading the one case that matters.
		out += " protect_last_known_good=false"
	}
	return out
}

// unitSuffix renders a look-back unit compactly, spelling a custom period
// as its actual length rather than the bare word "days", which would read
// identically for a ten-day and a fortnightly chain.
func unitSuffix(unit string, periodDays int) string {
	if unit == config.GranularityDays && periodDays > 0 {
		return fmt.Sprintf("x%dd", periodDays)
	}
	return " " + unit
}
