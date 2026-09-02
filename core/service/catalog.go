package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/spdrman/rclone-manager/core/internal/app"
)

// CatalogReport is the outcome of a catalog scan or rebuild across every
// configured backup set (FR-9's journal, reconstructed from the non-secret
// sidecar recovery manifests every committed artifact carries).
//
// Scanned/Reconstructed/AlreadyPresent are the three numbers an operator
// deciding whether to run the real thing actually needs: how much was
// found, how much of it the journal is missing, and how much it already
// has. Failures names the sidecar manifests that could not be read at all,
// which is the one outcome a "reconstructed N artifacts" line would
// otherwise hide.
type CatalogReport struct {
	// DryRun is true for a scan: nothing was written.
	DryRun bool

	// Scanned is every sidecar manifest examined, whatever the verdict.
	Scanned int
	// Reconstructed is how many journal rows were (or, for a scan, would
	// be) rebuilt.
	Reconstructed int
	// AlreadyPresent is how many artifacts the journal already knew
	// about and were left untouched.
	AlreadyPresent int

	// Failures is one entry per manifest that could not be read or
	// applied, in the order they were met. A non-empty Failures with a
	// non-zero Reconstructed is an ordinary partial result, not a
	// contradiction: rebuild continues past a bad manifest rather than
	// abandoning the artifacts it can still recover.
	//
	// A sidecar that disagrees with a journal row that already exists
	// lands here too (FR-32). It is not a read failure, but it is a
	// manifest the pass could not apply, and it is the one outcome an
	// operator most needs to see: the alternative is a rebuild quietly
	// choosing the database's version of events over the disk's.
	Failures []CatalogFailure
}

// CatalogFailure is one sidecar manifest a catalog pass could not use.
type CatalogFailure struct {
	// BackupSetID is "source/set".
	BackupSetID string
	// Path is the manifest that failed. It is a server-side path, and it
	// is reported because it is the only thing that identifies which
	// manifest needs attention; an operator with shell access is the only
	// audience a catalog failure has.
	Path string
	// Reason is the failure, already rendered as a sentence.
	Reason string
}

// ScanCatalog reports what a rebuild would reconstruct and writes nothing.
//
// It is the same code path RebuildCatalog takes, with the dry-run flag
// set: internal/app.RebuildCatalog's own doc explains why the two modes
// share one implementation and, absent a crash between them, predict each
// other exactly. A preview computed by a second, separate implementation
// would be a preview of something other than what runs.
func (b *BackupService) ScanCatalog(ctx context.Context) (CatalogReport, error) {
	return b.catalogPass(ctx, true)
}

// RebuildCatalog reconstructs missing journal rows from the sidecar
// recovery manifests already on disk, for every configured backup set.
//
// It never contacts a configured remote: rebuild only reads manifests that
// are already local and writes to the local journal. It also never removes
// or overwrites a journal row that already exists (internal/app's
// CatalogRebuildAlreadyPresent verdict), so running it against a healthy
// journal is a no-op rather than a reset.
func (b *BackupService) RebuildCatalog(ctx context.Context) (CatalogReport, error) {
	return b.catalogPass(ctx, false)
}

func (b *BackupService) catalogPass(ctx context.Context, dryRun bool) (CatalogReport, error) {
	st := b.state.Load()
	out := CatalogReport{DryRun: dryRun}

	for _, src := range st.inner.Config.Sources {
		for _, bs := range src.BackupSets {
			report, err := st.inner.RebuildCatalog(ctx, bs.ID, dryRun)
			if err != nil {
				// One backup set failing outright (its local directory is
				// unreadable, say) does not invalidate what the others
				// recovered, so it is recorded and the pass continues.
				// Returning here would make a whole rebuild's success
				// depend on the least healthy set in the deployment.
				out.Failures = append(out.Failures, CatalogFailure{
					BackupSetID: bs.ID.String(),
					Reason:      err.Error(),
				})
				continue
			}
			for _, f := range report.Findings {
				out.Scanned++
				switch f.Action {
				case app.CatalogRebuildReconstructed:
					out.Reconstructed++
				case app.CatalogRebuildAlreadyPresent:
					out.AlreadyPresent++
				case app.CatalogRebuildConflict:
					// A conflicting sidecar is a manifest this pass could
					// not apply, which is what Failures means, and it must
					// not be silently absent from the report: FR-32's whole
					// point is that a disagreement between a sidecar and a
					// journal row is reported rather than resolved. It is
					// deliberately NOT counted as AlreadyPresent, which
					// reads as "the journal already had this and all is
					// well".
					out.Failures = append(out.Failures, CatalogFailure{
						BackupSetID: bs.ID.String(),
						Path:        f.ManifestPath,
						Reason: fmt.Sprintf("%s already has a journal row and this sidecar disagrees with it; nothing was changed: %s",
							f.Artifact, strings.Join(f.Conflicts, "; ")),
					})
				}
			}
			for _, e := range report.Errors {
				out.Scanned++
				out.Failures = append(out.Failures, CatalogFailure{
					BackupSetID: bs.ID.String(),
					Path:        e.Path,
					Reason:      fmt.Sprint(e.Err),
				})
			}
		}
	}
	return out, nil
}
