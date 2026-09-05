package compat

import (
	"context"
	"fmt"
	"path/filepath"
)

// The orchestrator: one function that drives every surface FR-35 names and
// hands back the whole corpus, plus the path the checked-in baseline lives
// at.
//
// The cells are built in a fixed order because they are not independent.
// One seeded deployment feeds the row, schema, retention and prune cells,
// so they all describe the same ten artifacts rather than four fixtures
// that could drift apart, and the CLI cells need a binary built from this
// working tree before they can run anything.
//
// Every step returns an error rather than skipping a cell it could not
// capture. That is the shape Compare is built to punish from the other
// side: a cell that quietly went missing passes a comparison against
// nothing, so the capture refuses to produce a corpus with a hole in it in
// the first place.
//
// The cell names are numbered and are part of the checked-in corpus, which
// makes renaming one a two-sided edit: the old name reads as a cell that
// stopped being observed and the new one as a cell with no baseline, and
// Compare reports both.

// CorpusPath is where the checked-in baseline lives.
const CorpusPath = "testdata/medium-free-surfaces.json"

// CaptureAll drives every surface FR-35 names and returns the corpus this
// working tree produces.
//
// workDir is a throwaway directory this call owns completely; coreRoot is
// the core module root (so the CLI can be built from this tree); and
// fixtureDir holds the config corpus.
func CaptureAll(ctx context.Context, workDir, coreRoot, fixtureDir string) (Corpus, error) {
	corpus := Corpus{Cells: map[string]Cell{}}

	cfgCell, err := captureConfigValidation(fixtureDir)
	if err != nil {
		return corpus, fmt.Errorf("config validation cell: %w", err)
	}
	corpus.Cells["01-config-validation"] = cfgCell

	deployRoot := filepath.Join(workDir, "deployment")
	bs, _, err := seedDeployment(ctx, deployRoot, deploymentConfigYAML(deployRoot), theDeployment())
	if err != nil {
		return corpus, fmt.Errorf("seeding the deployment: %w", err)
	}
	dbPath := filepath.Join(deployRoot, "state.db")

	rowCell, err := captureArtifactRows(ctx, dbPath, deployRoot)
	if err != nil {
		return corpus, fmt.Errorf("artifact rows cell: %w", err)
	}
	corpus.Cells["02-artifact-rows-after-migration"] = rowCell

	schemaCell, err := captureSchema(ctx, dbPath)
	if err != nil {
		return corpus, fmt.Errorf("schema cell: %w", err)
	}
	corpus.Cells["03-migrated-schema"] = schemaCell

	keepCell, err := captureRetentionVerdicts(ctx, dbPath, bs)
	if err != nil {
		return corpus, fmt.Errorf("retention verdicts cell: %w", err)
	}
	corpus.Cells["04-retention-verdicts"] = keepCell

	pruneCell, err := capturePruneVerdicts(ctx, dbPath, deployRoot, bs)
	if err != nil {
		return corpus, fmt.Errorf("prune verdicts cell: %w", err)
	}
	corpus.Cells["05-prune-verdicts"] = pruneCell

	bin, err := buildCLI(coreRoot, workDir)
	if err != nil {
		return corpus, err
	}

	cliCell, usageCell, err := captureCLI(ctx, bin, filepath.Join(deployRoot, "config.yaml"), deployRoot)
	if err != nil {
		return corpus, fmt.Errorf("cli cell: %w", err)
	}
	corpus.Cells["06-cli-surfaces"] = cliCell
	corpus.Cells["06b-cli-usage-block"] = usageCell

	retCell, err := captureCLIRetention(ctx, bin, workDir)
	if err != nil {
		return corpus, fmt.Errorf("cli retention cell: %w", err)
	}
	corpus.Cells["07-cli-retention-preview"] = retCell

	// api/v1/openapi.json lives outside this Go module, and is read here
	// as a data file rather than imported, so nothing about the core
	// dependency rule changes. It survives both deletion proofs too:
	// verify-core-without-apps.sh deletes apps/, and
	// verify-core-without-distribution.sh deletes the distribution
	// adapter tree, and api/ is neither.
	contractCell, requestCell, err := captureAPIContract(filepath.Join(coreRoot, "..", "api", "v1", "openapi.json"))
	if err != nil {
		return corpus, fmt.Errorf("api contract cell: %w", err)
	}
	corpus.Cells["08-api-contract-promises"] = contractCell
	corpus.Cells["09-api-request-requirements"] = requestCell

	upgradedRows, upgradedVerdicts, err := captureUpgrade(ctx, workDir)
	if err != nil {
		return corpus, fmt.Errorf("upgrade cell: %w", err)
	}
	corpus.Cells["10-upgraded-artifact-rows"] = upgradedRows
	corpus.Cells["11-upgraded-retention-verdicts"] = upgradedVerdicts

	return corpus, nil
}
