// Can a run publish from somewhere other than the release branch?
//
// Issue #510 again, from the other end. Pinning the signing identity to
// @refs/heads/release is only true while `release` is the only ref that
// can push. `workflow_dispatch` can be aimed at any branch, and its
// `publish` input was taken as permission rather than as a request, so one
// dispatch from a feature branch would have pushed, signed and attested an
// image under that branch's OIDC identity: an artifact in a public
// registry that this project's own compliance record does not describe and
// the documented `cosign verify` rejects. Unlike a wrong string in a
// document, that one cannot be taken back cleanly.
//
// This does not pattern-match the shell. It extracts the `decide` step's
// own script out of the workflow and runs it, with the environment
// GitHub Actions would give it, once per ref and event worth asking about.
// A guard that is asserted by grep is a guard that survives being
// rewritten into something that no longer works, and the whole reason
// #510 existed is that two things nobody executed together had quietly
// stopped agreeing.
//
// GitHub Actions is effectively dispatch-only here and no gate can run
// this workflow, so this test is the only place the publish gate is ever
// actually executed.
package packaging

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// publishingRefName is the one ref a release may be published from. It is
// the branch half of SigningIdentity, and it is the same constant, so a
// move of one is a move of both.
const publishingRefName = "refs/heads/" + SigningWorkflowBranch

// releaseWorkflowDecideJob is the job that decides whether a run
// publishes. Every other job reads its output, which is what makes this
// one script worth executing.
type releaseWorkflowDecideJob struct {
	Jobs struct {
		Decide struct {
			Outputs map[string]string `yaml:"outputs"`
			Steps   []struct {
				ID  string            `yaml:"id"`
				Run string            `yaml:"run"`
				Env map[string]string `yaml:"env"`
			} `yaml:"steps"`
		} `yaml:"decide"`
	} `yaml:"jobs"`
}

func readDecideStep(t *testing.T) (script string, env map[string]string, outputs map[string]string) {
	t.Helper()
	raw, err := os.ReadFile(Path(SigningWorkflowPath))
	if err != nil {
		t.Fatalf("cannot read %s: %v", SigningWorkflowPath, err)
	}
	var wf releaseWorkflowDecideJob
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("cannot parse %s: %v", SigningWorkflowPath, err)
	}
	for _, step := range wf.Jobs.Decide.Steps {
		if step.ID == "decide" {
			if strings.TrimSpace(step.Run) == "" {
				t.Fatalf("%s has a decide step with no script, so nothing decides whether a run publishes", SigningWorkflowPath)
			}
			return step.Run, step.Env, wf.Jobs.Decide.Outputs
		}
	}
	t.Fatalf("%s has no step with id `decide` in the `decide` job. Every publishing step reads needs.decide.outputs.publish, so this test cannot check the gate that is actually used; rename it back or update this test to name the real one", SigningWorkflowPath)
	return "", nil, nil
}

// runDecide executes the real decide script the way Actions runs it and
// returns what it wrote to GITHUB_OUTPUT as publish.
func runDecide(t *testing.T, script, ref, eventName, inputPublish string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "decide.sh")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("cannot stage the decide script: %v", err)
	}
	outFile := filepath.Join(dir, "github_output")
	summaryFile := filepath.Join(dir, "step_summary")
	for _, f := range []string{outFile, summaryFile} {
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatalf("cannot stage %s: %v", f, err)
		}
	}

	// The same shell options Actions uses for a `run:` block on
	// ubuntu-latest, so a script that only works under a laxer shell is
	// not quietly accepted here.
	cmd := exec.Command("bash", "--noprofile", "--norc", "-e", "-o", "pipefail", path)
	cmd.Env = append(os.Environ(),
		"GITHUB_REF="+ref,
		"GITHUB_REF_NAME="+strings.TrimPrefix(strings.TrimPrefix(ref, "refs/heads/"), "refs/tags/"),
		"EVENT_NAME="+eventName,
		"INPUT_PUBLISH="+inputPublish,
		"GITHUB_OUTPUT="+outFile,
		"GITHUB_STEP_SUMMARY="+summaryFile,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the decide script failed on ref %q, event %q, publish input %q: %v\n%s", ref, eventName, inputPublish, err, out)
	}

	written, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("cannot read what the decide script wrote: %v", err)
	}
	var publish string
	var seen int
	for _, line := range strings.Split(string(written), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "publish="); ok {
			publish = rest
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("the decide script wrote %d publish= lines on ref %q, event %q, publish input %q; the job's output is one value and every publishing step reads it:\n%s",
			seen, ref, eventName, inputPublish, written)
	}
	return publish
}

// TestOnlyTheReleaseRefCanPublish runs the gate.
//
// The rows that matter are the ones with a publish input of "true" on a
// ref that is not the release branch: those are the one-click path to an
// image nobody can verify, and every one of them has to come back false.
func TestOnlyTheReleaseRefCanPublish(t *testing.T) {
	script, env, outputs := readDecideStep(t)

	// The script reads its event and input through these, so a rename
	// that this test did not follow would silently test an empty
	// environment and pass everything.
	if got := env["EVENT_NAME"]; !strings.Contains(got, "github.event_name") {
		t.Errorf("the decide step maps EVENT_NAME from %q; this test supplies EVENT_NAME as github.event_name, so it would be testing something else", got)
	}
	if got := env["INPUT_PUBLISH"]; !strings.Contains(got, "inputs.publish") {
		t.Errorf("the decide step maps INPUT_PUBLISH from %q; this test supplies INPUT_PUBLISH as inputs.publish, so it would be testing something else", got)
	}
	if got := outputs["publish"]; !strings.Contains(got, "steps.decide.outputs.publish") {
		t.Errorf("the decide job publishes output %q rather than steps.decide.outputs.publish, so the script this test runs is not the one the other jobs read", got)
	}
	if !strings.Contains(script, "GITHUB_REF") {
		t.Errorf("the decide script never looks at GITHUB_REF, so nothing stops a workflow_dispatch aimed at any branch from publishing. That is #510's other half: an image signed under a ref the compliance record does not describe")
	}

	cases := []struct {
		name  string
		ref   string
		event string
		input string
		want  string
		why   string
	}{
		{
			name: "a push to the release branch", ref: publishingRefName, event: "push", input: "", want: "true",
			why: "a merge to release is the deliberate act that publishes, and this is the only path that should reach the registry",
		},
		{
			name: "a confirmed dispatch on the release branch", ref: publishingRefName, event: "workflow_dispatch", input: "true", want: "true",
			why: "dispatch stays usable for a cut made by hand, on release, where the identity is the recorded one",
		},
		{
			name: "a dry-run dispatch on the release branch", ref: publishingRefName, event: "workflow_dispatch", input: "false", want: "false",
			why: "a dry run against the real tree is the point of keeping dispatch, and it must not publish",
		},
		{
			name: "a dispatch from main asking to publish", ref: "refs/heads/main", event: "workflow_dispatch", input: "true", want: "false",
			why: "this is the one-click path to an image signed as @refs/heads/main, which the recorded identity does not cover and the documented cosign verify rejects",
		},
		{
			name: "a dispatch from a feature branch asking to publish", ref: "refs/heads/fix/510-cosign-identity", event: "workflow_dispatch", input: "true", want: "false",
			why: "any branch can be dispatched from, and none of them but release is a reviewed cut",
		},
		{
			name: "a dispatch from a branch whose name starts with release", ref: "refs/heads/release-candidate", event: "workflow_dispatch", input: "true", want: "false",
			why: "the ref check has to be an equality, not a prefix; release-candidate is a different branch and signs under a different identity",
		},
		{
			name: "a dispatch from a tag asking to publish", ref: "refs/tags/v0.3.0", event: "workflow_dispatch", input: "true", want: "false",
			why: "a tag ref signs as @refs/tags/..., which is exactly the identity #510 wrongly documented and which nothing verifies against",
		},
		{
			name: "a push that somehow reached a branch other than release", ref: "refs/heads/main", event: "push", input: "", want: "false",
			why: "the trigger is the only thing keeping pushes off other branches, and a trigger is one line to edit; the gate must not depend on it",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runDecide(t, script, tc.ref, tc.event, tc.input)
			if got != tc.want {
				t.Errorf("ref %q, event %q, publish input %q decided publish=%q, want %q.\n%s",
					tc.ref, tc.event, tc.input, got, tc.want, tc.why)
			}
		})
	}
}

// TestThePublishGateAgreesWithTheSigningIdentity ties the ref this gate
// allows to the ref the signing identity claims, so the two cannot be
// moved apart. It is the same binding as the trigger check in
// signing_identity_test.go, applied to the gate rather than to the trigger.
func TestThePublishGateAgreesWithTheSigningIdentity(t *testing.T) {
	if !strings.HasSuffix(SigningIdentity, "@"+publishingRefName) {
		t.Errorf("the only ref that may publish is %q and SigningIdentity is %q. Every image this workflow pushes is signed under the ref it published from, so these two are the same fact written twice and they disagree",
			publishingRefName, SigningIdentity)
	}
}
