package machines

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheSourceMachineHasOneDefinition is #451's third acceptance criterion
// held as a test: two-machine-backup.sh and this harness build the source
// machine from one Dockerfile text.
//
// They do it by both reading the same file, so there is nothing to keep in
// sync, which is the point. What can still go wrong is somebody putting the
// Dockerfile back inline in either place, and this is what notices. It reads
// the shell script rather than trusting the comment in it, because a comment
// saying "shared with the harness" is exactly what survives a change that
// stops it being true.
func TestTheSourceMachineHasOneDefinition(t *testing.T) {
	root := repoRoot(t)
	const name = "source-machine.Dockerfile"
	path := filepath.Join(root, "scripts", "e2e", name)

	text, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the shared source machine Dockerfile is missing at %s: %v", path, err)
	}
	// The two properties the harness and the shell script both depend on.
	// Not the whole file: this is a guard against the definition moving,
	// not a copy of it in another language.
	if !strings.Contains(string(text), "FROM atmoz/sftp") {
		t.Errorf("%s does not build on atmoz/sftp, which is the chrooted, key-only sshd the whole tier's posture assumes:\n%s", name, text)
	}
	if !strings.Contains(string(text), "iptables") {
		t.Errorf("%s does not install iptables, so LimitConnections cannot impose #264's rule and every connection-cap test becomes a copy of the uncapped case:\n%s", name, text)
	}

	scriptPath := filepath.Join(root, "scripts", "e2e", "two-machine-backup.sh")
	script, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("reading %s: %v", scriptPath, err)
	}
	if !strings.Contains(string(script), name) {
		t.Errorf("%s never mentions %s, so the shell script is building its source machine from a definition of its own again. Two definitions of the simulated VPS agree right up until they do not, and the one that drifts is the one nobody is running that day.", filepath.Base(scriptPath), name)
	}
	// The inline heredoc it used to build from, named so a reader can tell
	// this apart from a rename.
	if strings.Contains(string(script), "RUN apk add --no-cache iptables\nDOCKERFILE") {
		t.Errorf("%s still builds its source machine from an inline Dockerfile heredoc, which is the second definition #451 removed", filepath.Base(scriptPath))
	}
}

// TestTheDriverRunsTheTierInsideAManagerMachine is the other half: the
// placement seam has a driver on the other side of it (#451), and that
// driver is what makes NetworkEnv something other than dead code.
//
// It asserts the shape rather than running it, because running it means
// standing up a manager container and this package's own tests are the ones
// running inside that container when it does. What it checks is that the
// driver sets the variable this package reads, does not publish a port, and
// keeps the root refusal in core/internal/testenv rather than opting out of
// it.
func TestTheDriverRunsTheTierInsideAManagerMachine(t *testing.T) {
	path := filepath.Join(repoRoot(t), "scripts", "e2e", "run-machine-tier.sh")
	script, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the machine-tier driver is missing at %s: %v", path, err)
	}
	text := string(script)

	if !strings.Contains(text, NetworkEnv) {
		t.Errorf("the driver never sets %s, so the tier inside it would publish loopback ports and reach its machines through Docker Desktop's port proxy, which is the placement #451 exists to replace", NetworkEnv)
	}
	if !strings.Contains(text, "--user") {
		t.Errorf("the driver does not run the manager machine as a named user. core/internal/testenv REFUSES to run as root rather than skipping the permission-bit tests, and a rootful manager would turn that refusal into a red gate or an opt-out")
	}
	if strings.Contains(text, "RCLONE_MANAGER_ALLOW_ROOT") {
		t.Errorf("the driver sets the root opt-out. That flag exists for a person who typed it on purpose, not for a driver to set on everybody's behalf: setting it here deletes eight permission-bit assertions from every run inside the manager")
	}
	if !strings.Contains(text, "cmd/gotestwatch") {
		t.Errorf("the driver runs the tier under a bare `go test`. A machine-tier package's wall clock tracks real machine load, so a fixed -timeout chosen on a quiet machine kills a run that is still making progress (#256), which is why scripts/ci-local.sh puts these packages under gotestwatch. A driver meant to stand in for that step has to keep the bound")
	}
	if !strings.Contains(text, "EXIT_CANNOT_RUN=3") {
		t.Errorf("the driver has no CANNOT RUN status. A machine that cannot run the tier is neither a pass nor a failure, and two-machine-backup.sh already ledgers that as exit 3; without it the gate cannot tell the two apart without parsing prose")
	}
}
