package spk

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The tests in this file run the shipped scripts through /bin/sh rather
// than reading them. Three of the properties this package's safety rests
// on are behaviours of shell code - a pid file that names somebody
// else's process, an unexported SYNOPKG_ variable, an unbounded log -
// and none of them can be established by grepping for a line.
//
// They run on a development host as readily as on a DSM unit, which is
// why pid_command has a ps fallback: /proc exists on the NAS and not on
// a Mac, and the question "what is this pid running?" has to be
// answerable in both places for the test to mean anything.

// stagedScripts writes the shipped scripts/ directory into a temp
// directory and returns it, so a test can run a stage the way DSM does,
// with common.sh sitting next to it.
func stagedScripts(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	names := append([]string{SharedScriptName}, LifecycleScriptNames...)
	for _, name := range names {
		body, err := assetFS.ReadFile("assets/scripts/" + name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o755); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

// runSh runs one script with a deliberately minimal environment: only
// what the test names is set, so a variable the script needs and does
// not assert is missing here exactly as it would be on a DSM build that
// does not export it.
func runSh(t *testing.T, script string, env []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("/bin/sh", append([]string{script}, args...)...)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, env...)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !asExitError(err, &exit) {
			t.Fatalf("run %s: %v", script, err)
		}
		code = exit.ExitCode()
	}
	return string(out), code
}

func asExitError(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}

// TestScripts_RefuseToRunWithoutTheSynopkgVariables is the assertion
// every other path guarantee in this package rests on.
//
// common.sh derives PKG_VAR, PKG_ETC and PKG_BIN from SYNOPKG_PKGNAME
// and SYNOPKG_PKGDEST. Unset, PKG_VAR becomes /var/packages//var, which
// is /var/packages/var: postinst would chmod 0750 a directory outside
// this package, start-stop-status would create pid files under it and
// delete files from it, and the static scan cannot see any of that
// because it reasons textually about ${SYNOPKG_PKGNAME} and never about
// whether the variable holds anything.
func TestScripts_RefuseToRunWithoutTheSynopkgVariables(t *testing.T) {
	dir := stagedScripts(t)

	for _, name := range append([]string{SharedScriptName}, LifecycleScriptNames...) {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read staged %s: %v", name, err)
		}
		if name != SharedScriptName && !strings.Contains(string(body), SharedScriptName) {
			// The three no-op stages source nothing and derive nothing,
			// so there is no variable for them to assert.
			continue
		}
		t.Run(name, func(t *testing.T) {
			out, code := runSh(t, filepath.Join(dir, name), nil, "start")
			if code == 0 {
				t.Fatalf("%s ran to success with no SYNOPKG_PKGNAME and no SYNOPKG_PKGDEST:\n%s", name, out)
			}
			if !strings.Contains(out, "SYNOPKG_PKGNAME") {
				t.Fatalf("%s exited %d but never said which variable was missing:\n%s", name, code, out)
			}
		})
	}
}

// TestCommonSh_GuardIsWhatStopsIt is the positive control for the test
// above: those scripts would exit non-zero on a development host for
// several reasons that have nothing to do with the guard, so the guard
// has to be shown to be the thing that stopped them.
//
// The probe derives a path and prints it, and creates nothing, so the
// control can be run with the guard removed without touching anything
// outside the temp directory.
func TestCommonSh_GuardIsWhatStopsIt(t *testing.T) {
	dir := stagedScripts(t)
	shared := filepath.Join(dir, SharedScriptName)
	body, err := os.ReadFile(shared)
	if err != nil {
		t.Fatalf("read common.sh: %v", err)
	}

	probe := filepath.Join(dir, "probe")
	if err := os.WriteFile(probe, []byte(
		". \""+shared+"\"\necho \"PKG_VAR=${PKG_VAR}\"\n"), 0o755); err != nil {
		t.Fatalf("write probe: %v", err)
	}

	out, code := runSh(t, probe, nil)
	if code == 0 || strings.Contains(out, "PKG_VAR=") {
		t.Fatalf("common.sh derived a path from unset variables (exit %d):\n%s", code, out)
	}

	// Now the same probe against a common.sh with the two assertions
	// taken out. It has to reach the derivation and produce exactly the
	// path the guard exists to prevent, or this test is proving nothing.
	ungated := stripGuards(string(body))
	if ungated == string(body) {
		t.Fatal("could not find the SYNOPKG_ assertions in common.sh, so this control proves nothing")
	}
	if err := os.WriteFile(shared, []byte(ungated), 0o755); err != nil {
		t.Fatalf("write ungated common.sh: %v", err)
	}
	out, code = runSh(t, probe, nil)
	if code != 0 || !strings.Contains(out, "PKG_VAR=/var/packages//var") {
		t.Fatalf("without the guard the probe should have derived /var/packages//var (exit %d):\n%s", code, out)
	}
}

func stripGuards(body string) string {
	var kept []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, `: "${SYNOPKG_`) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// daemonFixture writes a script that behaves like one of the package's
// daemons for the purposes of these tests: it stays alive, and it
// records the fact that it was signalled.
func daemonFixture(t *testing.T, path string) string {
	t.Helper()
	signalled := path + ".signalled"
	body := fmt.Sprintf(`#!/bin/sh
trap 'echo signalled > %q; exit 0' TERM
i=0
while [ $i -lt 600 ]; do
    sleep 0.1
    i=$((i + 1))
done
`, signalled)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return signalled
}

// TestPidAlive_ChecksIdentityNotExistence covers the failure that needs
// only one unclean shutdown to reach.
//
// var/ survives reboots, upgrades and uninstalls, so a pid file outlives
// the pid space that wrote it. With a bare `kill -0`, status then
// reports the package Running against whatever the kernel handed that
// number to next - DSM shows Running, nothing is running, backups stop
// with no error anywhere - and stop signals and then SIGKILLs an
// unrelated process.
func TestPidAlive_ChecksIdentityNotExistence(t *testing.T) {
	dir := stagedScripts(t)
	dest := t.TempDir()
	ours := filepath.Join(dest, "bin", "backup-manager-web")
	daemonFixture(t, ours)
	decoy := filepath.Join(dest, "elsewhere", "somebody-elses-daemon")
	daemonFixture(t, decoy)

	env := []string{
		"SYNOPKG_PKGNAME=" + PackageName,
		"SYNOPKG_PKGDEST=" + dest,
	}
	probe := filepath.Join(dir, "probe")
	if err := os.WriteFile(probe, []byte(`
. "$(dirname "$0")/common.sh"
# The daemon's own output goes nowhere: it inherits this shell's stdout,
# and the Go side reads that to completion, so a fixture left holding the
# pipe would make the test wait out the fixture's whole lifetime.
"$1" >/dev/null 2>&1 &
echo $! > "$2"
sleep 1
if pid_alive "$2"; then echo VERDICT=ours; else echo VERDICT=not-ours; fi
# SIGKILL, not SIGTERM: the fixture traps SIGTERM to record that it was
# signalled, and this cleanup must not be mistaken for the thing under
# test in the stop_daemon case below.
kill -9 %1 2>/dev/null
`), 0o755); err != nil {
		t.Fatalf("write probe: %v", err)
	}

	for _, tc := range []struct {
		name    string
		command string
		want    string
	}{
		// The baseline. Without it, "not-ours" for the decoy would be
		// indistinguishable from a pid_alive that never says yes.
		{"the package's own daemon", ours, "VERDICT=ours"},
		{"a recycled pid running something else", decoy, "VERDICT=not-ours"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pidfile := filepath.Join(t.TempDir(), "engine.pid")
			out, code := runSh(t, probe, env, tc.command, pidfile)
			if code != 0 {
				t.Fatalf("probe exited %d:\n%s", code, out)
			}
			if !strings.Contains(out, tc.want) {
				t.Fatalf("pid_alive against %s: want %s, got:\n%s", tc.command, tc.want, out)
			}
		})
	}
}

// TestStopDaemon_DoesNotSignalSomebodyElsesProcess is the consequence of
// the test above, at the place where it costs something: stop runs
// before every upgrade and uninstall, and an operator can press it at
// any time.
func TestStopDaemon_DoesNotSignalSomebodyElsesProcess(t *testing.T) {
	dir := stagedScripts(t)
	dest := t.TempDir()
	ours := filepath.Join(dest, "bin", "backup-manager-web")
	oursSignalled := daemonFixture(t, ours)
	decoy := filepath.Join(dest, "elsewhere", "somebody-elses-daemon")
	decoySignalled := daemonFixture(t, decoy)

	// start-stop-status' dispatch runs a stage, and this test wants the
	// two functions above it. Cutting at the dispatch is exact, and the
	// harness sits in the same directory so its own `. common.sh` works
	// the way the stage's does.
	stage, err := os.ReadFile(filepath.Join(dir, "start-stop-status"))
	if err != nil {
		t.Fatalf("read start-stop-status: %v", err)
	}
	const dispatch = "case \"$1\" in"
	cut := strings.Index(string(stage), dispatch)
	if cut < 0 {
		t.Fatalf("start-stop-status no longer dispatches on %q, so this harness is wrong", dispatch)
	}
	harness := filepath.Join(dir, "harness")
	if err := os.WriteFile(harness, append([]byte(string(stage)[:cut]), []byte(`
"$1" >/dev/null 2>&1 &
echo $! > "$2"
sleep 1
stop_daemon "$2" "the daemon"
kill -9 %1 2>/dev/null
`)...), 0o755); err != nil {
		t.Fatalf("write harness: %v", err)
	}

	env := []string{
		"SYNOPKG_PKGNAME=" + PackageName,
		"SYNOPKG_PKGDEST=" + dest,
	}
	for _, tc := range []struct {
		name         string
		command      string
		signalled    string
		wantSignal   bool
		otherProcess string
	}{
		// The control: a stop_daemon that signals nothing at all would
		// pass the decoy case for the wrong reason.
		{"the package's own daemon is stopped", ours, oursSignalled, true, ""},
		{"a recycled pid is left alone", decoy, decoySignalled, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pidfile := filepath.Join(t.TempDir(), "engine.pid")
			out, code := runSh(t, harness, env, tc.command, pidfile)
			if code != 0 {
				t.Fatalf("harness exited %d:\n%s", code, out)
			}
			// The trap writes its file from a signal handler, so give it
			// a moment before deciding it never ran.
			deadline := time.Now().Add(2 * time.Second)
			got := false
			for time.Now().Before(deadline) {
				if _, err := os.Stat(tc.signalled); err == nil {
					got = true
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			if got != tc.wantSignal {
				t.Fatalf("signalled=%v, want %v (output:\n%s)", got, tc.wantSignal, out)
			}
		})
	}
}

// TestBoundLogs_CapsEachLogAtOneGeneration covers the other thing var/
// surviving forever costs: both daemons append to a log on the DSM
// system volume, with no rotation anywhere in the package, and a full
// system volume takes down every package on the NAS.
func TestBoundLogs_CapsEachLogAtOneGeneration(t *testing.T) {
	dir := stagedScripts(t)
	logs := t.TempDir()
	big := filepath.Join(logs, "engine.log")
	small := filepath.Join(logs, "ui.log")
	if err := os.WriteFile(big, []byte(strings.Repeat("x", 4096)), 0o644); err != nil {
		t.Fatalf("write engine.log: %v", err)
	}
	if err := os.WriteFile(small, []byte("still small\n"), 0o644); err != nil {
		t.Fatalf("write ui.log: %v", err)
	}

	probe := filepath.Join(dir, "probe")
	if err := os.WriteFile(probe, []byte(`
. "$(dirname "$0")/common.sh"
ENGINE_LOG="$1"
UI_LOG="$2"
LOG_MAX_BYTES=1024
bound_logs
`), 0o755); err != nil {
		t.Fatalf("write probe: %v", err)
	}
	out, code := runSh(t, probe, []string{
		"SYNOPKG_PKGNAME=" + PackageName,
		"SYNOPKG_PKGDEST=" + t.TempDir(),
	}, big, small)
	if code != 0 {
		t.Fatalf("probe exited %d:\n%s", code, out)
	}

	if info, err := os.Stat(big); err != nil || info.Size() != 0 {
		t.Fatalf("engine.log was not truncated (err=%v, size=%v)", err, sizeOf(info))
	}
	rotated, err := os.ReadFile(big + ".1")
	if err != nil || len(rotated) != 4096 {
		t.Fatalf("engine.log.1 should hold the 4096 bytes that were rotated out (err=%v, got %d)", err, len(rotated))
	}
	// The control: a bound_logs that truncated unconditionally would
	// pass everything above and still lose the UI log on every poll.
	kept, err := os.ReadFile(small)
	if err != nil || string(kept) != "still small\n" {
		t.Fatalf("ui.log was under the ceiling and should have been left alone (err=%v, got %q)", err, string(kept))
	}
	if _, err := os.Stat(small + ".1"); err == nil {
		t.Fatal("ui.log was rotated even though it was under the ceiling")
	}
}

func sizeOf(info os.FileInfo) any {
	if info == nil {
		return "missing"
	}
	return info.Size()
}
