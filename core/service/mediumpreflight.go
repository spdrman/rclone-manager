package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/mediumcheck"
)

// ErrMediumNotFound is what PreflightStorageMedium returns for a medium id
// the running configuration does not declare.
//
// Its own sentinel, like ErrBackupSetNotFound, because it is a state an
// operator reaches by typing rather than a bug: a medium is declared in
// the configuration file and nowhere else, so a name that was right last
// week is a name somebody removed, and the answer wants to be "that is not
// one of your mediums" rather than an internal error.
var ErrMediumNotFound = errors.New("service: storage medium not found")

// MediumPreflight is one preflight, as this boundary reports it.
//
// It is a projection rather than internal/mediumcheck.Report handed
// through, for the reason every other type in this package is one:
// internal packages are free to change shape, and a boundary that
// re-exported one would make an engine refactor an API break. What it
// carries is deliberately the same three things per step, though, because
// the whole value of the check is in which step failed and how.
type MediumPreflight struct {
	// Medium is the medium id this ran against.
	Medium string

	// OK is true when nothing failed.
	OK bool

	// Checks is one entry per step, in the engine's own order.
	Checks []MediumPreflightCheck
}

// MediumPreflightCheck is one step's result.
//
// There is no field here for key material of any kind, and there never
// will be (FR-33). Detail is one of the engine's own sentences and never
// an underlying error's text: what actually came back names a path on the
// host or the name of an environment variable, which goes to this
// manager's log instead. See internal/mediumcheck's package doc.
type MediumPreflightCheck struct {
	// Step names what this check proves: "credentials", "reach",
	// "deliverable", "write", "read_back", "storage_class",
	// "verification", "delete".
	Step string

	// Outcome is "passed", "failed" or "skipped". Skipped is a real
	// answer and not a quiet pass: it means an earlier step failed in a
	// way that makes this one meaningless, and a surface that renders a
	// skipped write as anything but "this was never tried" has told
	// somebody their bucket is writable on the strength of a credential
	// that was never obtained.
	Outcome string

	// Category is the transport category a failure classified as, or
	// empty. It is the machine-readable half: a client branches on this
	// and never on Detail.
	Category string

	// Detail is a sentence for a person.
	Detail string
}

// PreflightStorageMedium proves one declared storage medium actually
// works, before a cycle carrying a real backup finds out for an operator
// (issue #443).
//
// A medium that does not work is a SUCCESSFUL call with a report saying
// so, exactly like TestConnection reports a bad host through its result
// rather than through an error: a bucket that is not there is what an
// operator did, not what broke. The error return is for a medium this
// configuration does not declare, and for an instance with no way to reach
// one at all.
//
// It writes a probe object and deletes it. That is a real side effect on
// somebody's bucket, which is why nothing schedules this and why the route
// that reaches it carries CSRF.
func (b *BackupService) PreflightStorageMedium(ctx context.Context, id string) (MediumPreflight, error) {
	report, err := b.state.Load().inner.PreflightMedium(ctx, id)
	switch {
	case app.AsMediumNotDeclared(err):
		return MediumPreflight{}, fmt.Errorf("%w: %s", ErrMediumNotFound, id)
	case err != nil:
		return MediumPreflight{}, fmt.Errorf("service: preflighting storage medium %s: %w", id, err)
	}
	return toMediumPreflight(report), nil
}

// toMediumPreflight is the one translation, so the wire shape and the
// engine's shape have exactly one place they can drift apart in.
func toMediumPreflight(report mediumcheck.Report) MediumPreflight {
	out := MediumPreflight{Medium: report.Medium, OK: report.OK, Checks: make([]MediumPreflightCheck, 0, len(report.Checks))}
	for _, c := range report.Checks {
		out.Checks = append(out.Checks, MediumPreflightCheck{
			Step:     string(c.Step),
			Outcome:  string(c.Outcome),
			Category: c.Category,
			Detail:   c.Detail,
		})
	}
	return out
}
