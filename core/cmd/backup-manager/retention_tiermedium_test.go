package main

import (
	"strings"
	"testing"
)

// `backup-manager retention --tier` and the destination it could not name
// (EPIC G, G2.3, issue #595).
//
// RetentionTier.Medium's own doc deferred this: "an override replaces the
// file's chain with an all-local one. That is inert while nothing reads
// this field, and it is #239's to answer when retention starts planning on
// it." #239 landed, so the deferral expired, and what was inert became a
// preview that quietly answers a different question from the one asked.
//
// Every test here drives the real command against a deployment whose daily
// tier really does live on a medium (writeOffsiteTestConfig, from
// retention_placement_test.go), because that is the only deployment where
// the difference between the two answers is visible at all.

// TestRun_TierOverrideCannotSilentlyMoveTheChainOntoLocal is the bug,
// pinned as behaviour so it cannot come back by accident.
//
// The deployment sends daily offsite. An operator previewing a longer
// daily window with --tier is asking "what would this chain keep", not
// "what would this chain keep if you also moved everything back onto the
// NAS", and before --tier-medium existed there was no way to say the first
// one: the override replaced the whole chain with an all-local one and the
// placement plan under it was computed against local, with nothing in the
// output saying so.
//
// This asserts the FIXED shape of that: the override still replaces the
// chain (that part is not a bug and applyRetentionOverrides' own doc
// argues why a merge would be worse), and the operator is TOLD that the
// chain they supplied names no destination while the deployment declares
// one, so the placement plan below it is not the deployment's.
func TestRun_TierOverrideCannotSilentlyMoveTheChainOntoLocal(t *testing.T) {
	configPath := writeOffsiteTestConfig(t)
	if got := run([]string{"run", "--config", configPath}); got != 1 {
		t.Fatalf("run: %d, want 1: the move it planned cannot reach its medium", got)
	}

	var out string
	err := captureStderr(t, func() {
		out = captureStdout(t, func() {
			if got := run([]string{"retention", "--config", configPath, "--dry-run", "--tier", "daily:day:7"}); got != 0 {
				t.Fatalf("retention --tier: %d, want 0", got)
			}
		})
	})

	if strings.Contains(out, "cold_offsite") {
		t.Errorf("the preview under an all-local --tier chain still names the deployment's medium, so it is not previewing the chain it was given.\ngot:\n%s", out)
	}
	for _, want := range []string{"--tier-medium", "cold_offsite"} {
		if !strings.Contains(err, want) {
			t.Errorf("nothing on stderr mentions %q; a chain typed at the command line that names no destination, beside a deployment that declares one, previews placement somewhere other than where the copies actually are, and the operator has to be told that before they read the plan.\ngot:\n%s", want, err)
		}
	}
}

// TestRun_TierMediumPreviewsAgainstTheRealMediumChain is the fix, and the
// assertion that matters: not that the flag parses, but that the preview
// under it names the move the deployment's own chain would make.
//
// A test that only checked --tier-medium was accepted would pass against
// the bug, because the bug is not a parse failure: it is a preview that
// computes placement against local while printing no less confidently.
func TestRun_TierMediumPreviewsAgainstTheRealMediumChain(t *testing.T) {
	configPath := writeOffsiteTestConfig(t)
	if got := run([]string{"run", "--config", configPath}); got != 1 {
		t.Fatalf("run: %d, want 1: the move it planned cannot reach its medium", got)
	}

	out := captureStdout(t, func() {
		got := run([]string{
			"retention", "--config", configPath, "--dry-run",
			"--tier", "daily:day:14",
			"--tier-medium", "daily=cold_offsite",
		})
		if got != 0 {
			t.Fatalf("retention --tier --tier-medium: %d, want 0", got)
		}
	})

	if !strings.Contains(out, "MOVE") {
		t.Fatalf("the preview under --tier-medium names no move at all, so it is still planning an all-local chain.\ngot:\n%s", out)
	}
	for _, want := range []string{"backup.dump", "local", "cold_offsite"} {
		if !strings.Contains(out, want) {
			t.Errorf("the preview does not mention %q; --tier-medium exists so that a chain typed at the command line can say where the copies go, and the placement plan has to be computed against that.\ngot:\n%s", want, out)
		}
	}
}

// TestRun_TierMediumIsRefusedWhenNoTierOfThatNameWasGiven: the value is
// attached to the thing it belongs to, so a name attached to nothing is a
// mistake rather than a no-op.
//
// Silently ignoring it is the failure mode this whole issue is about: an
// operator who typed `--tier-medium monthy=cold_offsite` would get a
// confident all-local preview for the tier they meant.
func TestRun_TierMediumIsRefusedWhenNoTierOfThatNameWasGiven(t *testing.T) {
	configPath := writeOffsiteTestConfig(t)

	err := captureStderr(t, func() {
		got := run([]string{
			"retention", "--config", configPath, "--dry-run",
			"--tier", "daily:day:7",
			"--tier-medium", "monthy=cold_offsite",
		})
		if got == 0 {
			t.Fatalf("retention with a --tier-medium naming no tier exited 0; it has to be refused")
		}
	})
	for _, want := range []string{"monthy", "names no tier this command line gave", "daily"} {
		if !strings.Contains(err, want) {
			t.Errorf("the refusal does not mention %q, so it does not say which name went unmatched or what was on offer.\ngot:\n%s", want, err)
		}
	}
}

// TestRun_TierMediumWithNoTierAtAll: --tier-medium is a modifier on
// --tier, so on a command line with no chain on it there is nothing for it
// to modify, and accepting it would teach an operator it did something.
func TestRun_TierMediumWithNoTierAtAll(t *testing.T) {
	configPath := writeOffsiteTestConfig(t)

	err := captureStderr(t, func() {
		if got := run([]string{"retention", "--config", configPath, "--dry-run", "--tier-medium", "daily=cold_offsite"}); got == 0 {
			t.Fatalf("retention with --tier-medium and no --tier exited 0; it has to be refused")
		}
	})
	for _, want := range []string{"-tier-medium", "no -tier was given"} {
		if !strings.Contains(err, want) {
			t.Errorf("the refusal does not mention %q.\ngot:\n%s", want, err)
		}
	}
}

// TestRun_TierMediumRefusalsComeFromTheConfigLayer is the "one rule in one
// place" half. --tier-medium hands its value on unparsed, so a medium that
// is not declared, and the reserved local id, are refused in the words
// config.Validate uses for the identical mistake written into config.yaml.
// A copy of either rule here would be a second rule free to disagree with
// the first.
func TestRun_TierMediumRefusalsComeFromTheConfigLayer(t *testing.T) {
	configPath := writeOffsiteTestConfig(t)

	for _, tc := range []struct {
		name  string
		spec  string
		wants []string
	}{
		{
			name:  "a medium no storage_mediums entry declares",
			spec:  "daily=not_declared",
			wants: []string{"not_declared", "not declared by any storage_mediums entry", "no fall-back to local"},
		},
		{
			name:  "the reserved local id, which is unspellable",
			spec:  "daily=local",
			wants: []string{"local", "implicit local medium", "omit the medium key"},
		},
		{
			name:  "a medium id that is not lower_snake_case",
			spec:  "daily=Cold-Offsite",
			wants: []string{"Cold-Offsite", "lower_snake_case"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := captureStderr(t, func() {
				got := run([]string{
					"retention", "--config", configPath, "--dry-run",
					"--tier", "daily:day:7",
					"--tier-medium", tc.spec,
				})
				if got == 0 {
					t.Fatalf("retention --tier-medium %s exited 0; it has to be refused", tc.spec)
				}
			})
			for _, want := range tc.wants {
				if !strings.Contains(err, want) {
					t.Errorf("the refusal does not mention %q, so it is not the config layer's own message.\ngot:\n%s", want, err)
				}
			}
		})
	}
}
