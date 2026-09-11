package cliapi_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/backupdproject/backupd/apps/common/auth/local"
	"github.com/backupdproject/backupd/apps/common/platform/profile"
	"github.com/backupdproject/backupd/apps/common/webhost/serve"
	"github.com/backupdproject/backupd/core/apicontract"
	"github.com/backupdproject/backupd/core/service"
)

// Issue #545, the last of #536: the two routes an operator can change this
// product through are one route, driven rather than described.
//
// # Why the proof lives here and nowhere else
//
// The client that carries a routed command is core/internal/apiclient. It
// is under core/internal, so apps/ can never import it, and core/ may never
// import apps/ (scripts/architecture/check-core-dependency-rule.sh). So
// there is no package anywhere that can hold both the client and the real
// router, the real authentication, the real CSRF middleware and the real
// UI-host reverse proxy at once. #543 and #544 both ran their proofs
// against a stand-in engine for exactly that reason, and both said so.
//
// A test can still put the two together, as long as it does not import
// them both: this package runs the real backupd BINARY as a
// subprocess, and stands up the real apps/common/webhost/serve engine and
// the real UI host in process. Nothing here is a stand-in. The CSRF cookie
// is minted by apps/common/csrf, the session by apps/common/auth/local, the
// paths are matched by the real chi router, and a request aimed at the
// published port goes through the same StripUntrustedIdentity,
// SecurityHeaders and EnsureCSRFCookie chain a browser's does.
//
// The neighbouring file is issue #167's equivalence check and set the
// pattern: build the CLI, stand up the real HTTP surface, drive both, and
// compare. This is the same idea one step further on, because since #543
// and #544 the CLI is not a second implementation of the same decision, it
// is a caller of the first one.
//
// # What this establishes, stated no more strongly than it is
//
// The issue's words are that a CLI mutation must be "visible over HTTP and
// in the Web UI without a restart". That is proved below in the only sense
// available: the change is made BY the engine, so the process serving the
// Web UI holds it the moment the command exits, and its own config_revision
// moves. What is not proved, because it is not true, is that the two routes
// render the same answer to every read. #544 found that all four read
// surfaces print things api/v1/openapi.json cannot express, and answered
// the question rather than the rendering. So the honest claim, and the one
// TestTheTwoRoutesCannotDisagreeAboutTheConfiguration drives, is narrower:
// the two routes cannot disagree about the CONFIGURATION they answer from,
// and the fields they still cannot be compared on are enumerated in
// unreportedOnTheWire below, as an executed list rather than a paragraph.
//
// One test here records a gap rather than a property and says so in its
// own name: a read on a deployment that has not been told where its engine
// is announces `unconfirmed` rather than being prevented, which is the
// shipped container's own default.
//
// There used to be a second, and #555 closed what it recorded. A routed
// write compared nothing at all, so one wrong character in
// $BACKUP_MANAGER_API_URL sent the change into a different deployment's
// engine and both surfaces reported success. It now asks the engine which
// deployment it serves before it sends anything and refuses when that is
// not the deployment the command was typed at, which is what
// TestARoutedWriteRefusesWhenTheEngineServesADifferentDeployment drives.
// What it compares is a deployment identity rather than a revision, for
// the reason that test spells out: a revision is a hash of configuration
// content, so a staging and a production instance built from one template
// share one, and those two are exactly the pair an address gets confused
// between.
//
// # The Web UI itself
//
// A browser is not driven from here. The Web UI's data is these responses:
// it holds no configuration of its own, and backupdproject/backupd-tests
// Suite B is what drives the rendered thing. What is driven here is the
// exact HTTP surface the browser talks to, through the published port,
// which is the half of "visible in the Web UI" that can be wrong.

// The administrator these tests enroll. A fixed pair, generated nowhere
// near a real deployment, held in this process and in the CLI subprocess's
// own environment, and written to disk only as apps/common/auth/local's own
// Argon2id hash, which is what the product does with any password.
const (
	testAdmin    = "cliapi-operator"
	testPassword = "not-a-real-password-either"
)

// aKnownHostsLine is a syntactically real known_hosts line for a host
// nothing here ever dials. Every create below carries one because the
// trust anchor has to be decided before a backup set is persisted, and
// carrying it is also what keeps a routed create from making an outbound
// SSH connection from a test.
const aKnownHostsLine = "[source.example.internal]:2222 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJ7Zq1i0i7Xw3v0m7d3Wl1nZk5Q9tJm2fVYy0m9c8ZqR"

// stack is one deployment with an engine serving it: the announcement a
// CLI probes for, the real /api/v1, and the real UI host in front of it.
type stack struct {
	configPath string
	engineURL  string
	uiURL      string
}

// startStack brings up everything a real deployment has except the
// container boundary, over the configuration configPath names.
//
// The order is the order apps/generic/cmd/backupd-web uses and it
// is load-bearing: AnnounceServing before service.Open, so a CLI that
// arrives mid-start finds the announcement rather than a half-open
// journal. core/service's liveengine.go has the whole arrangement.
func startStack(t *testing.T, configPath string) *stack {
	t.Helper()

	release, err := service.AnnounceServing(configPath)
	if err != nil {
		t.Fatalf("announcing this process as serving %s: %v", configPath, err)
	}
	t.Cleanup(func() { _ = release() })

	backend, closeFn, err := service.Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("service.Open(%s): %v", configPath, err)
	}
	t.Cleanup(func() { _ = closeFn() })

	// One store path, resolved once. Two calls to t.TempDir() are two
	// directories, so reading it twice would give this Service a store the
	// administrator was never written to and every login would fail for a
	// reason that has nothing to do with what is under test.
	storePath := filepath.Join(t.TempDir(), "local-auth.json")
	// The same provisioning path `backupd-web auth create-admin`
	// takes, rather than the bootstrap-token enrolment the neighbouring
	// file uses: this test needs a username and password to hand the CLI,
	// and that is the command an operator runs to get one.
	if _, err := local.CreateAdmin(local.CreateAdminConfig{
		StorePath: storePath,
		Username:  testAdmin,
		Password:  testPassword,
	}); err != nil {
		t.Fatalf("CreateAdmin: %v", err)
	}
	authSvc, err := local.New(local.Config{StorePath: storePath})
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}
	adapter, err := profile.Generic.Profile().Adapter(profile.AdapterConfig{LocalAuth: authSvc.Authenticator()})
	if err != nil {
		t.Fatalf("Adapter: %v", err)
	}

	engine := httptest.NewServer(serve.NewEngine(serve.EngineConfig{
		Platform:   adapter,
		AuthRoutes: authSvc.Handler(),
		Backend:    backend,
	}))
	t.Cleanup(engine.Close)

	upstream, err := url.Parse(engine.URL)
	if err != nil {
		t.Fatalf("parsing the engine's own address: %v", err)
	}
	ui := httptest.NewServer(serve.NewUI(serve.UIConfig{
		Upstream: upstream,
		// The app shell, not the real bundle. What is under test here is
		// the /api/v1 half of that handler and the middleware around it;
		// which files the static half serves is apps/generic/tests/uibundle's.
		StaticFS: fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<!doctype html>")}},
	}))
	t.Cleanup(ui.Close)

	return &stack{configPath: configPath, engineURL: engine.URL, uiURL: ui.URL}
}

// browser is what the Web UI is, from the outside: a cookie jar, a session,
// and the double-submit token every state-changing request has to echo.
type browser struct {
	t      *testing.T
	client *http.Client
	base   string
}

// signIn signs in at base exactly the way the login page does.
func signIn(t *testing.T, base string) *browser {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	b := &browser{t: t, client: &http.Client{Jar: jar}, base: base}

	// The CSRF cookie has to exist before a POST can echo it, and only a
	// response from this host can mint one.
	seed, err := b.client.Get(base + "/health/live")
	if err != nil {
		t.Fatalf("seeding the CSRF cookie: %v", err)
	}
	seed.Body.Close()

	body, _ := json.Marshal(map[string]string{"username": testAdmin, "password": testPassword})
	req, err := http.NewRequest(http.MethodPost, base+"/api/v1/auth/login", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(local.CSRFHeaderName, b.csrf())
	resp, err := b.client.Do(req)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("login at %s returned %d: %s", base, resp.StatusCode, raw)
	}
	return b
}

// csrf reads the double-submit token this host issued.
func (b *browser) csrf() string {
	b.t.Helper()
	u, err := url.Parse(b.base)
	if err != nil {
		b.t.Fatalf("parsing %s: %v", b.base, err)
	}
	for _, c := range b.client.Jar.Cookies(u) {
		if c.Name == local.CSRFCookieName {
			return c.Value
		}
	}
	b.t.Fatalf("no %s cookie from %s, so a state-changing request cannot be made", local.CSRFCookieName, b.base)
	return ""
}

// get reads one JSON response into out.
func (b *browser) get(path string, out any) {
	b.t.Helper()
	resp, err := b.client.Get(b.base + path)
	if err != nil {
		b.t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		b.t.Fatalf("GET %s returned %d: %s", path, resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		b.t.Fatalf("decoding %s: %v\n%s", path, err, raw)
	}
}

// backupSets is what the Web UI's own list shows, as ids.
func (b *browser) backupSets() []string {
	b.t.Helper()
	var answer apicontract.ListBackupSetsResponse
	b.get("/api/v1/backup-sets", &answer)
	ids := make([]string, 0, len(answer.BackupSets))
	for _, bs := range answer.BackupSets {
		ids = append(ids, bs.ID)
	}
	sort.Strings(ids)
	return ids
}

// configRevision is the engine's own hash of the configuration it is
// serving from. It is the one fact both routes can compare across every
// field, including the four api/v1/openapi.json cannot carry, and it is
// what moves when the engine adopts a change without being restarted.
func (b *browser) configRevision() string {
	b.t.Helper()
	var answer apicontract.VersionResponse
	b.get("/api/v1/system/version", &answer)
	if answer.ConfigRevision == "" {
		b.t.Fatal("the engine reported an empty config_revision, so nothing below is comparing anything")
	}
	return answer.ConfigRevision
}

// deploymentID is the engine's own answer to "which deployment am I",
// which is a different fact from the revision above and the one a routed
// write is checked against. It is minted once beside the journal and does
// not move when the configuration does, so two instances built from one
// template are told apart by it and are not told apart by a revision.
func (b *browser) deploymentID() string {
	b.t.Helper()
	var answer apicontract.VersionResponse
	b.get("/api/v1/system/version", &answer)
	if answer.DeploymentID == "" {
		b.t.Fatal("the engine reported an empty deployment_id, so a routed write has nothing to check itself against")
	}
	return answer.DeploymentID
}

// invocation is one run of the real binary.
type invocation struct {
	argv   []string
	code   int
	stdout string
	stderr string
}

func (r invocation) String() string {
	return fmt.Sprintf("backupd %s\nexit %d\nstdout:\n%s\nstderr:\n%s",
		strings.Join(r.argv, " "), r.code, r.stdout, r.stderr)
}

// output is both streams, for the assertions that care that an operator
// saw a line rather than which descriptor carried it.
func (r invocation) output() string { return r.stdout + r.stderr }

// runCLI runs the real binary with the route settings env carries.
//
// The environment is built from scratch rather than inherited, so a
// developer with $BACKUP_MANAGER_API_URL exported for their own deployment
// runs the same suite CI does. That is route.go's clearInheritedRouteSettings
// one process boundary out.
func runCLI(t *testing.T, bin string, env map[string]string, argv ...string) invocation {
	t.Helper()
	cmd := exec.Command(bin, argv...)
	cmd.Env = append(os.Environ(),
		"BACKUP_MANAGER_API_URL=",
		"BACKUP_MANAGER_API_USERNAME=",
		"BACKUP_MANAGER_API_PASSWORD=",
	)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	var exitErr *exec.ExitError
	if err != nil {
		if !asExitError(err, &exitErr) {
			t.Fatalf("running %v: %v", argv, err)
		}
		code = exitErr.ExitCode()
	}
	return invocation{argv: argv, code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func asExitError(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}

// routeTo is the environment that makes engine-attached mode carryable.
func routeTo(base string) map[string]string {
	return map[string]string{
		"BACKUP_MANAGER_API_URL":      base,
		"BACKUP_MANAGER_API_USERNAME": testAdmin,
		"BACKUP_MANAGER_API_PASSWORD": testPassword,
	}
}

// writePrivateKey writes a throwaway SSH private key for a create to
// import. Generated per call and never reused, so nothing here is a
// credential that outlives one test.
func writePrivateKey(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// createArgs is one complete `backup-set create`, so a test that is about
// where the change LANDS does not restate nine flags that are not the point.
func createArgs(configPath, keyPath, id string, extra ...string) []string {
	return append([]string{
		"backup-set", "--config", configPath, "create", id,
		"--host", "source.example.internal",
		"--port", "2222",
		"--user", "backupuser",
		"--ssh-key-file", keyPath,
		"--known-hosts-line", aKnownHostsLine,
		"--remote-path", "/srv/backups",
		// A fixed absolute path rather than one derived from the fixture's
		// own directory, so the same create typed at two deployments
		// prints the same line and the two can be compared at all
		// (TestWhatARoutedCommandStillCannotReport). Nothing is ever
		// written there: no cycle runs for a set created in these tests.
		"--local-path", "/data/backups/api",
		"--completion-strategy", "rename",
		// Issue #624: `create` proves the connection before it writes,
		// and source.example.internal is a name that resolves nowhere.
		// Every test in this package is about where the change LANDS, on
		// which route, in which mode, so they skip the check the way an
		// operator building configuration offline does. Whether the check
		// happens at all is core/cmd/backupd's own suite, and
		// whether it happens against two real machines is
		// scripts/e2e/two-machine-backup.sh.
		"--no-verify",
	}, extra...)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return string(raw)
}

// newSetID is the backup set every routed create below adds. It is not in
// writeFixture's configuration, and each test asserts that before it starts.
const newSetID = "api/postgres"

// TestARoutedMutationIsVisibleOverHTTPWithoutARestart is #545's first
// proof, and #535's exact scenario run forwards.
//
// #535 was a `backup-set create` through `docker exec` against a container
// that was already serving: the file changed, the engine had read it an
// hour earlier, and the Web UI went on showing the old world until somebody
// restarted the container. Here the same command is typed against the same
// running engine and the set is in the engine's own answer before the
// command's exit code has been read.
//
// Both addresses an operator can plausibly use are driven, because they are
// different amounts of machinery and only one of them had ever been tried.
// The engine's own listener is what `docker exec` into the engine container
// reaches. The published port is the UI host, which is the only container
// with a published port at all, so it is what a NAS shell reaches; that
// path adds StripUntrustedIdentity, SecurityHeaders, a reverse proxy and a
// SECOND EnsureCSRFCookie in front of the engine's own, and a cold request
// through it carries two Set-Cookie: bm_csrf headers on one response. PR
// #546's review flagged that as untested and unreachable from where the
// client lives. It is reachable from here.
//
// What this proves, exactly: the change was made by the process that serves
// the Web UI, so there is no second writer for the Web UI to be out of date
// with. What it does not prove is that a read renders identically on both
// routes; that is TestTheTwoRoutesCannotDisagreeAboutTheConfiguration's,
// and it is a weaker claim on purpose.
func TestARoutedMutationIsVisibleOverHTTPWithoutARestart(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the real CLI against a real engine")
	}
	bin := buildCLI(t, repoRoot(t))

	for _, target := range []struct {
		name string
		pick func(*stack) string
	}{
		{"through the engine's own address", func(s *stack) string { return s.engineURL }},
		{"through the published port the Web UI is served on", func(s *stack) string { return s.uiURL }},
	} {
		t.Run(target.name, func(t *testing.T) {
			_, configPath := writeFixture(t)
			live := startStack(t, configPath)
			view := signIn(t, live.uiURL)

			before := view.backupSets()
			revisionBefore := view.configRevision()
			if contains(before, newSetID) {
				t.Fatalf("the fixture already has %s, so this test cannot show it arriving: %v", newSetID, before)
			}

			got := runCLI(t, bin, routeTo(target.pick(live)), createArgs(configPath, writePrivateKey(t), newSetID)...)
			if got.code != 0 {
				t.Fatalf("a routed create was refused:\n%s", got)
			}
			if !strings.Contains(got.output(), "mode: engine-attached") {
				t.Errorf("the command did not announce engine-attached mode, so it may not have gone anywhere near the engine:\n%s", got)
			}
			if !strings.Contains(got.output(), "hands the change to it at") {
				t.Errorf("the command announced engine-attached mode without saying it handed the change over:\n%s", got)
			}

			// Nothing was restarted and nothing was reopened: this is the
			// same handler over the same *service.BackupService the
			// assertions above already read through.
			after := view.backupSets()
			if !contains(after, newSetID) {
				t.Fatalf("the CLI reported success and the process serving the Web UI does not have %s, which is #535 exactly.\nbefore: %v\nafter:  %v\n%s", newSetID, before, after, got)
			}
			if len(after) != len(before)+1 {
				t.Errorf("the engine's list moved by more than the one set this command created\nbefore: %v\nafter:  %v", before, after)
			}
			if revision := view.configRevision(); revision == revisionBefore {
				t.Errorf("the engine is still serving configuration %s after a create it accepted, so it has adopted the change nowhere an operator can see", revision)
			}
		})
	}
}

// TestAnUnroutedMutationBesideALiveEngineRefusesAndTheWebUIIsUnchanged is
// the control the test above needs, and it is #535 prevented rather than
// #535 fixed.
//
// It is the shipped container's own default: nothing sets
// $BACKUP_MANAGER_API_URL, so a `docker exec ... backupd backup-set
// create` finds a serving engine, has no route to it, and stops. That is
// worth driving on its own, because the first proof passes on a deployment
// that has been told where its engine is and most have not been.
func TestAnUnroutedMutationBesideALiveEngineRefusesAndTheWebUIIsUnchanged(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the real CLI against a real engine")
	}
	bin := buildCLI(t, repoRoot(t))

	_, configPath := writeFixture(t)
	live := startStack(t, configPath)
	view := signIn(t, live.uiURL)

	before := view.backupSets()
	fileBefore := readFile(t, configPath)

	// No route in the environment at all, which is what a container that
	// nobody has configured looks like.
	got := runCLI(t, bin, nil, createArgs(configPath, writePrivateKey(t), newSetID)...)
	if got.code == 0 {
		t.Fatalf("a create beside a live engine with no route to it exited 0, so it wrote the file behind that engine's back:\n%s", got)
	}
	// The code, not merely a nonzero one. #551 gave this refusal a code of
	// its own so a script never has to read the prose, and the second
	// create on a fresh install has to reach the same one the first now
	// does (TestAFirstCreateFromTheCLICannotLandBehindTheSetupFlow), or an
	// operator's script would branch differently on the same news.
	if got.code != exitEngineHoldsDeployment {
		t.Errorf("the refusal exited %d, want %d:\n%s", got.code, exitEngineHoldsDeployment, got)
	}
	if !strings.Contains(got.output(), "mode: engine-attached") {
		t.Errorf("the refusal did not say which mode it was in:\n%s", got)
	}
	if !strings.Contains(got.output(), "refused here rather than downgraded") {
		t.Errorf("the refusal did not say that nothing was written:\n%s", got)
	}
	if after := readFile(t, configPath); after != fileBefore {
		t.Error("a refused create changed config.yaml underneath the running engine")
	}
	if after := view.backupSets(); !equal(after, before) {
		t.Errorf("the engine's world changed on a command that refused\nbefore: %v\nafter:  %v", before, after)
	}
}

// TestTheTwoRoutesCannotDisagreeAboutTheConfiguration is the honest
// statement of what #544 built, driven against the real engine rather than
// against a stand-in for it.
//
// The wording of #545 implies that a read is answered over the wire. It is
// not, and #544 explains at length why not: every one of the four read
// surfaces prints something api/v1/openapi.json cannot express, so a
// wire-rendered CLI would print LESS exactly when an engine is up. What is
// compared instead is the engine's own config_revision against the one this
// command computed from the file it loaded, and unequal revisions are a
// refusal before a line is printed.
//
// So the claim being driven here is: the two routes cannot disagree about
// the configuration they answer from. This drives both halves of it, and
// the second half is #535 from the reading side, arranged deliberately.
func TestTheTwoRoutesCannotDisagreeAboutTheConfiguration(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the real CLI against a real engine")
	}
	bin := buildCLI(t, repoRoot(t))

	t.Run("agreeing, a read says it is answering about the engine's world", func(t *testing.T) {
		_, configPath := writeFixture(t)
		live := startStack(t, configPath)

		got := runCLI(t, bin, routeTo(live.engineURL), "sources", "--config", configPath)
		if got.code != 0 {
			t.Fatalf("`sources` beside an engine holding the identical configuration was refused:\n%s", got)
		}
		if !strings.Contains(got.stderr, "mode: engine-attached") {
			t.Errorf("`sources` did not announce that it had checked its answer against the engine:\n%s", got)
		}
		if !strings.Contains(got.stdout, "production/pg") {
			t.Errorf("`sources` announced engine-attached mode and printed no backup sets:\n%s", got)
		}
	})

	t.Run("disagreeing, a read refuses rather than describing a world nobody serves", func(t *testing.T) {
		_, configPath := writeFixture(t)
		live := startStack(t, configPath)

		// #535 arranged on purpose: the file gains a backup set the
		// running engine read its configuration too early to know about,
		// and nothing re-reads that file. Before #544 this printed two
		// backup sets while the Web UI showed one.
		behindTheEngine := strings.Replace(readFile(t, configPath),
			"retention:\n", "  - id: staging\n    backup_sets: []\nretention:\n", 1)
		if behindTheEngine == readFile(t, configPath) {
			t.Fatal("the fixture's shape changed and this edit no longer diverges anything, so the case below would pass for the wrong reason")
		}
		if err := os.WriteFile(configPath, []byte(behindTheEngine), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		got := runCLI(t, bin, routeTo(live.engineURL), "sources", "--config", configPath)
		if got.code == 0 {
			t.Fatalf("`sources` printed a world the serving process does not have, and exited 0:\n%s", got)
		}
		if !strings.Contains(got.stderr, "is holding a different configuration") {
			t.Errorf("the refusal did not name the divergence:\n%s", got)
		}
		if strings.Contains(got.stdout, "staging") {
			t.Errorf("the refusal still printed the diverged world:\n%s", got)
		}
	})
}

// fixture is the one deployment a row is driven against, and the operands
// that deployment can supply.
type fixture struct {
	configPath string
	setID      string
	artifactID string
	keyPath    string
}

// surface is one invocation of one dispatched verb.
//
// Every verb the binary dispatches has at least one row, and the coverage
// guard below asserts that against the binary's own usage block rather than
// against a list typed here. A verb that arrives with no row fails, which is
// the tripwire half of #545's third proof: a command nobody declared cannot
// be driven, and a command nobody drives cannot be shown to behave the same
// on both routes.
type surface struct {
	// verb is the top-level command this row exercises, spelled the way
	// the dispatch spells it.
	verb string

	// name distinguishes rows that share a verb.
	name string

	// argv is the whole invocation, including the binary's own flags.
	argv func(f fixture) []string

	// writes says this invocation changes config.yaml when nothing is
	// serving the deployment. It is what keeps the guard from passing
	// because an argv turned out to be inert.
	writes bool

	// routed says this invocation goes through the write door, so a
	// serving engine carries it and an unreachable one refuses it. False
	// on a write is a gap, and why is recorded in note rather than left to
	// a reader.
	//
	// It used to be readable as "a serving engine can carry this WRITE",
	// because every row that had it was a write. #636 moved `medium
	// preflight` onto that door without making it one: a check that passes
	// clears a destination's unverified mark, which is a configuration
	// write, but the invocation this table drives names a destination
	// nothing declares and so writes nothing. The field is what it always
	// checked, which is the door, and the reads test below reads it that
	// way.
	routed bool

	// mode is the announcement this invocation makes with nothing serving:
	// "direct" for the commands that name one, and "" for the commands
	// that still name none.
	mode string

	// exit is the code this invocation exits with, with nothing serving.
	exit int

	// needsArtifacts asks for one real cycle before the row runs, so the
	// operand it names exists.
	needsArtifacts bool

	// skipDirect is for the one command that does not exit on its own.
	skipDirect bool

	// note explains a row that does not exit 0, is not routed, or is not
	// driven. A row with a surprising declaration and no note is a row
	// somebody should have to justify in review.
	note string
}

// surfaces is every dispatched verb, driven.
//
// The exit codes are what this deployment actually produces, not what the
// verb produces in general: five of these name a subject a fixture with two
// good backups does not have (a quarantined artifact, a FAILED one, a
// configured storage medium, an archived copy), and what their rows prove
// is that they refuse for the missing subject and never for a route.
var surfaces = []surface{
	{verb: "run", name: "one cycle", mode: "", exit: 0,
		argv: func(f fixture) []string { return []string{"run", "--config", f.configPath} }},
	{verb: "daemon", name: "one engine per deployment", skipDirect: true,
		note: "it does not exit on its own, so the direct arm cannot drive it; beside a live engine it refuses at once, which is the arm that matters here",
		argv: func(f fixture) []string { return []string{"daemon", "--config", f.configPath} }},
	{verb: "check", name: "config and journal", mode: "", exit: 0,
		argv: func(f fixture) []string { return []string{"check", "--config", f.configPath} }},
	{verb: "status", name: "health", mode: "direct", exit: 0, needsArtifacts: true,
		argv: func(f fixture) []string { return []string{"status", "--config", f.configPath} }},
	{verb: "sources", name: "the configured sets", mode: "direct", exit: 0,
		argv: func(f fixture) []string { return []string{"sources", "--config", f.configPath} }},
	{verb: "backup-set", name: "create", mode: "direct", exit: 0, writes: true, routed: true,
		argv: func(f fixture) []string { return createArgs(f.configPath, f.keyPath, newSetID) }},
	{verb: "backup-set", name: "patch", mode: "direct", exit: 0, writes: true, routed: true,
		argv: func(f fixture) []string {
			return []string{"backup-set", "--config", f.configPath, "patch", f.setID, "--stale-after", "48h"}
		}},
	{verb: "backup-set", name: "remove", mode: "direct", exit: 0, writes: true, routed: true,
		argv: func(f fixture) []string {
			return []string{"backup-set", "--config", f.configPath, "remove", f.setID}
		}},
	{verb: "backup-set", name: "retention, setting a policy", mode: "direct", exit: 0, writes: true, routed: false,
		note: "#543 left this unrouted on purpose: the policy is a whole chain read from flags or stdin and the preview beside it is #544's, so routing half of it would be worse than routing none",
		argv: func(f fixture) []string {
			// A whole chain, because an override replaces the
			// deployment's whole chain rather than merging with it, and a
			// partial one is refused at load. Which is the row saying
			// something true about the verb: this is not a flag edit.
			return []string{"backup-set", "--config", f.configPath, "retention", f.setID,
				"--daily-days", "5", "--weekly-months", "3", "--monthly-months", "12"}
		}},
	{verb: "artifacts", name: "the whole journal", mode: "direct", exit: 0, needsArtifacts: true,
		argv: func(f fixture) []string { return []string{"artifacts", "--config", f.configPath} }},
	{verb: "activity", name: "the durable log", mode: "direct", exit: 0, needsArtifacts: true,
		argv: func(f fixture) []string { return []string{"activity", "--config", f.configPath} }},
	{verb: "activity", name: "the live feed, which only a serving engine has", skipDirect: true, exit: 0,
		note: "--follow reads the feed the serving process holds in memory, so unlike the durable log above it has no direct answer at all: a deployment with nothing serving it has no live feed to show, which is why this row is routed-only",
		argv: func(f fixture) []string {
			return []string{"activity", "--config", f.configPath, "--follow", "--limit", "1"}
		}},
	{verb: "fetch", name: "one set on demand", mode: "", exit: 0,
		argv: func(f fixture) []string {
			return []string{"fetch", "--config", f.configPath, "--source", "production", "--backup-set", "pg"}
		}},
	{verb: "retention", name: "the preview", mode: "direct", exit: 0, needsArtifacts: true,
		argv: func(f fixture) []string { return []string{"retention", "--config", f.configPath, "--dry-run"} }},
	{verb: "reconcile", name: "FR-17", mode: "", exit: 0,
		argv: func(f fixture) []string { return []string{"reconcile", "--config", f.configPath} }},
	{verb: "validate", name: "one durable copy", mode: "", exit: 0, needsArtifacts: true,
		argv: func(f fixture) []string { return []string{"validate", "--config", f.configPath, f.artifactID} }},
	{verb: "catalog", name: "rebuild, previewed", mode: "", exit: 0, needsArtifacts: true,
		argv: func(f fixture) []string {
			return []string{"catalog", "--config", f.configPath, "rebuild", "--dry-run"}
		}},
	{verb: "quarantine", name: "revalidate", mode: "", exit: 1, needsArtifacts: true,
		note: "nothing here is quarantined, so this refuses for the missing subject",
		argv: func(f fixture) []string {
			return []string{"quarantine", "--config", f.configPath, "revalidate", f.artifactID}
		}},
	{verb: "unconfigured", name: "what the journal remembers", mode: "", exit: 0,
		argv: func(f fixture) []string { return []string{"unconfigured", "--config", f.configPath} }},
	{verb: "medium", name: "preflight", mode: "direct", exit: 1, routed: true,
		note: "this deployment declares no storage medium, so this refuses for the missing subject. " +
			"It names a mode as of #636 and did not before: a check that PASSES now clears that destination's " +
			"unverified mark, which is a configuration write, so the verb moved onto the door the medium writes " +
			"already go through. It is not a write ITSELF, which is why this row does not carry `writes`: the one " +
			"thing it can change is a mark, and only on a destination that both exists and passes",
		argv: func(f fixture) []string {
			return []string{"medium", "--config", f.configPath, "preflight", "no-such-medium"}
		}},
	{verb: "retry", name: "one failed backup", mode: "", exit: 1, needsArtifacts: true,
		note: "nothing here is FAILED, so this refuses for the missing subject",
		argv: func(f fixture) []string { return []string{"retry", "--config", f.configPath, f.artifactID} }},
	{verb: "restore", name: "one archived copy", mode: "", exit: 1, needsArtifacts: true,
		note: "nothing here is on a storage medium, so this refuses for the missing subject",
		argv: func(f fixture) []string {
			return []string{"restore", "--config", f.configPath, f.artifactID, "--medium", "no-such-medium", "--acknowledge"}
		}},
	{verb: "settings", name: "the live policy", mode: "", exit: 0,
		note: "the settings READ is not routed. #543 routed the write and left the read where it was, so this is one of the reads that still names no mode at all",
		argv: func(f fixture) []string { return []string{"settings", "--config", f.configPath} }},
	{verb: "settings", name: "patch", mode: "direct", exit: 0, writes: true, routed: true,
		argv: func(f fixture) []string {
			return []string{"settings", "--config", f.configPath, "patch", "--timezone", "Europe/Berlin"}
		}},
	{verb: "version", name: "the build stamp", mode: "", exit: 0,
		argv: func(_ fixture) []string { return []string{"version"} }},
}

// newFixture lays down one deployment for one row.
//
// Every row gets its own, because half of them change the configuration and
// a shared one would make each row's result depend on the order the rows
// happened to run in.
func newFixture(t *testing.T, bin string, s surface) fixture {
	t.Helper()
	_, configPath := writeFixture(t)
	f := fixture{
		configPath: configPath,
		setID:      "production/pg",
		artifactID: "production/pg/db-2026-08-01.dump",
		keyPath:    writePrivateKey(t),
	}
	if s.needsArtifacts {
		// One real cycle, so the operand this row names is a backup that
		// exists rather than a string. A row whose subject is missing
		// refuses for the wrong reason and proves nothing about routes.
		if got := runCLI(t, bin, nil, "run", "--config", configPath); got.code != 0 {
			t.Fatalf("the warm-up cycle this row needs did not run:\n%s", got)
		}
	}
	return f
}

// TestEveryDispatchedVerbHasARow is the tripwire half of #545's third
// proof: a command that arrives with no row here cannot have been driven
// on either route.
//
// The verbs are read out of the binary's own usage block rather than typed
// here, so a verb that lands over there fails here without anybody
// remembering this file exists. What makes that sound is
// core/cmd/backupd's TestUsage_NamesEveryTopLevelCommand, which pins
// the usage block against the dispatch map itself; without it a verb could be
// dispatchable and unlisted, and this would be blind to exactly the verb
// nobody had thought about. The same blindness is why `backup-set remove`
// shipped undiscoverable (issue #391).
func TestEveryDispatchedVerbHasARow(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the real CLI")
	}
	bin := buildCLI(t, repoRoot(t))

	dispatched := verbsFromUsage(t, bin)
	if len(dispatched) < 15 {
		t.Fatalf("read %d verbs out of the usage block, which is too few to be the whole binary; the parser below has stopped matching: %v", len(dispatched), dispatched)
	}

	declared := map[string]bool{}
	for _, s := range surfaces {
		declared[s.verb] = true
	}
	for _, verb := range dispatched {
		if !declared[verb] {
			t.Errorf("the binary dispatches %q and no row here drives it, so nothing has ever checked what it does beside a running engine", verb)
		}
	}
	for verb := range declared {
		if !contains(dispatched, verb) {
			t.Errorf("a row here drives %q and the binary's usage block does not list it, so either the row is stale or the verb is undiscoverable", verb)
		}
	}
}

// verbsFromUsage reads the top-level verbs out of the usage block, which is
// what the binary prints when it is given nothing.
func verbsFromUsage(t *testing.T, bin string) []string {
	t.Helper()
	got := runCLI(t, bin, nil)
	if got.code != 2 {
		t.Fatalf("running the binary with no arguments exited %d, want 2 (a usage error):\n%s", got.code, got)
	}
	seen := map[string]bool{}
	var verbs []string
	for _, line := range strings.Split(got.output(), "\n") {
		if !strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "   ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		verb := fields[0]
		if seen[verb] || !isVerbName(verb) {
			continue
		}
		seen[verb] = true
		verbs = append(verbs, verb)
	}
	sort.Strings(verbs)
	return verbs
}

// isVerbName keeps the parser above from reading a wrapped continuation
// line as a verb. Verbs are lower case with hyphens and nothing else.
func isVerbName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && r != '-' {
			return false
		}
	}
	return true
}

// TestEveryCommandStillWorksWithNothingServingAndNamesItsMode is #545's
// second proof.
//
// Two things per row: the command does what it does on a host with nothing
// serving, which is the case the direct path exists for; and it says which
// mode it was in. The second half is where the issue's wording is stronger
// than the product. "Every command names the mode it used" is not true and
// is not claimed here: the four read surfaces and the five configuration
// writes name one, and the other eleven rows name none, which is recorded
// per row rather than papered over. mode.go's own closing note says the
// same thing in prose; this is the executed version of it, so a command
// that starts or stops naming a mode moves a row here.
func TestEveryCommandStillWorksWithNothingServingAndNamesItsMode(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the real CLI")
	}
	bin := buildCLI(t, repoRoot(t))

	for _, s := range surfaces {
		if s.skipDirect {
			continue
		}
		t.Run(s.verb+" "+s.name, func(t *testing.T) {
			f := newFixture(t, bin, s)
			before := readFile(t, f.configPath)

			got := runCLI(t, bin, nil, s.argv(f)...)
			if got.code != s.exit {
				t.Fatalf("exited %d, want %d\n%s", got.code, s.exit, got)
			}
			// Nothing is serving, so nothing may report otherwise. This is
			// the assertion that would catch a detector that says yes to
			// everything, which would strand this binary on every host
			// that has no engine at all.
			if strings.Contains(got.output(), "mode: engine-attached") {
				t.Errorf("this command found an engine on a deployment nothing is serving:\n%s", got)
			}
			if s.mode == "" {
				if strings.Contains(got.output(), "mode: ") {
					t.Errorf("this row is recorded as naming no mode and it named one; move the row rather than the assertion:\n%s", got)
				}
			} else if !strings.Contains(got.output(), "mode: "+s.mode) {
				t.Errorf("this command did not announce %q:\n%s", "mode: "+s.mode, got)
			}

			after := readFile(t, f.configPath)
			switch {
			case s.writes && after == before:
				t.Errorf("this row is recorded as a configuration write and config.yaml did not move, so the guard beside a live engine would pass for the wrong reason:\n%s", got)
			case !s.writes && after != before:
				t.Errorf("this row is not recorded as a configuration write and it changed config.yaml:\n%s", got)
			}
		})
	}
}

// TestNoCommandChangesTheConfigurationBesideAnEngineItCannotReach is #545's
// third proof, and it is the one that fails for a command that exists on
// one route and not the other.
//
// The invariant is universal and needs no per-row honesty: while something
// has announced that it serves this deployment and this command has no
// route to it, NO verb may change config.yaml. That is #535 stated over the
// whole command set rather than over the five verbs somebody remembered.
// A new command that writes the file through core/service directly, with no
// engine counterpart and no mode, is exactly what breaks it.
//
// The two arms have to be read together. This one alone is satisfied by an
// argv that does nothing at all, which is why every row that writes is also
// driven with nothing serving in the test above and required to move the
// file there. Together they say: this invocation can change the file, and
// beside an engine it does not.
func TestNoCommandChangesTheConfigurationBesideAnEngineItCannotReach(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the real CLI")
	}
	bin := buildCLI(t, repoRoot(t))

	for _, s := range surfaces {
		t.Run(s.verb+" "+s.name, func(t *testing.T) {
			f := newFixture(t, bin, s)

			// The announcement alone, with no HTTP surface behind it. That
			// is what the probe reads, and it is also the honest shape of
			// the case: a `backupd daemon` serves no HTTP at all,
			// and an engine whose address nobody has configured is
			// indistinguishable from one for a command with no route.
			release, err := service.AnnounceServing(f.configPath)
			if err != nil {
				t.Fatalf("announcing this process as serving %s: %v", f.configPath, err)
			}
			defer func() { _ = release() }()

			before := readFile(t, f.configPath)
			got := runCLI(t, bin, nil, s.argv(f)...)
			if after := readFile(t, f.configPath); after != before {
				t.Fatalf("this command changed config.yaml while another process was serving this deployment and it had no route to hand the change over. That is issue #535.\n%s", got)
			}
			if s.writes && got.code == 0 {
				t.Errorf("a configuration write beside an unreachable engine exited 0, so a script cannot tell it was refused:\n%s", got)
			}
		})
	}
}

// TestEveryConfigurationWriteEitherReachesTheEngineOrRefuses is the other
// half of the third proof, and the place the remaining gaps are recorded as
// something that runs.
//
// A write that this build routes has to reach the engine and move the
// configuration the engine is serving from. A write that this build does
// not route has to refuse, with nothing written on either side. There is no
// third outcome, and a row that changed category without its note being
// rewritten fails here rather than in an operator's deployment.
func TestEveryConfigurationWriteEitherReachesTheEngineOrRefuses(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the real CLI against a real engine")
	}
	bin := buildCLI(t, repoRoot(t))

	var writes int
	for _, s := range surfaces {
		if !s.writes {
			continue
		}
		writes++
		t.Run(s.verb+" "+s.name, func(t *testing.T) {
			f := newFixture(t, bin, s)
			live := startStack(t, f.configPath)
			view := signIn(t, live.uiURL)

			before := readFile(t, f.configPath)
			revisionBefore := view.configRevision()

			got := runCLI(t, bin, routeTo(live.engineURL), s.argv(f)...)
			revisionAfter := view.configRevision()

			if s.routed {
				if got.code != 0 {
					t.Fatalf("a write this build routes was refused:\n%s", got)
				}
				if !strings.Contains(got.output(), "hands the change to it at") {
					t.Errorf("the command did not say it had handed the change over:\n%s", got)
				}
				if revisionAfter == revisionBefore {
					t.Errorf("the engine is serving the same configuration %s it was before a write it accepted, so the Web UI still shows the old world:\n%s", revisionAfter, got)
				}
				return
			}

			if s.note == "" {
				t.Fatal("a write with no route and no note is a gap nobody has justified; write the reason into the row")
			}
			if got.code == 0 {
				t.Fatalf("a write this build does not route exited 0 beside a live engine, so it went into the file the engine will never re-read:\n%s", got)
			}
			if !strings.Contains(got.output(), "refused here rather than downgraded") {
				t.Errorf("the refusal did not say that nothing was written:\n%s", got)
			}
			if after := readFile(t, f.configPath); after != before {
				t.Error("a refused write changed config.yaml anyway")
			}
			if revisionAfter != revisionBefore {
				t.Errorf("the engine's configuration moved on a command that refused: %s then %s", revisionBefore, revisionAfter)
			}
		})
	}
	if writes < 5 {
		t.Fatalf("only %d rows are recorded as configuration writes; every one of the five this binary has should be here, and a table that has lost one proves less than it reads as proving", writes)
	}
}

// contains reports whether items holds want.
func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

// equal compares two id lists as they are printed, sorted by their callers.
func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// unreportedOnTheWire is every field the same command prints on the direct
// route and cannot print through the engine, as a list that runs.
//
// It is empty, and that is a fact worth keeping executable rather than
// deleting. It had one entry, stale_after: api/v1/openapi.json's BackupSet
// carried no such property, even though BackupSetSpec accepted one on the
// way in and UpdateBackupSetRequest could change it, so the API could write
// a field it could not read back and a routed create printed "not reported"
// for a value the operator had just typed. #555 put stale_after_seconds on
// the wire and the list emptied.
//
// Empty means the test below asserts something stronger than it used to:
// the two routes print the same report, line for line. An entry added here
// again is somebody recording a new divergence and having to say what it
// is, which is the shape this list existed for.
var unreportedOnTheWire = []string{}

// TestWhatARoutedCommandStillCannotReport drives the list above rather than
// leaving it as a paragraph, in both directions.
//
// The same create is typed at two deployments, one with nothing serving and
// one with an engine serving it, and the two reports are compared line for
// line. With the list empty the two have to match exactly, and with an
// entry on it every line that differs has to be about something on the
// list and every entry has to account for a line that differs.
//
// Both halves matter over time and they matter in opposite directions.
// Adding a divergence without recording it fails. Closing one in the
// contract and leaving the list alone also fails, until somebody shortens
// it, which is the opposite of how a documented limitation usually ages:
// this one has already been shortened to nothing that way.
func TestWhatARoutedCommandStillCannotReport(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the real CLI against a real engine")
	}
	bin := buildCLI(t, repoRoot(t))

	// The same input, all the way down: the same id, the same window, the
	// same paths, and the same key file. Two deployments that start out
	// saying the same thing.
	//
	// The same key matters and was found the hard way. Two calls to
	// writePrivateKey are two keys and two fingerprints, and the create
	// prints the fingerprint it imported, so the comparison reported a
	// divergence that was entirely its own.
	key := writePrivateKey(t)
	create := func(configPath string) []string {
		return createArgs(configPath, key, newSetID, "--stale-after", "48h")
	}

	_, directConfig := writeFixture(t)
	direct := runCLI(t, bin, nil, create(directConfig)...)
	if direct.code != 0 {
		t.Fatalf("the direct create failed, so there is nothing to compare against:\n%s", direct)
	}

	_, routedConfig := writeFixture(t)
	live := startStack(t, routedConfig)
	routed := runCLI(t, bin, routeTo(live.engineURL), create(routedConfig)...)
	if routed.code != 0 {
		t.Fatalf("the routed create failed, so there is nothing to compare:\n%s", routed)
	}

	onlyDirect, onlyRouted := reportDiff(t, direct.stdout, routed.stdout)
	if len(unreportedOnTheWire) == 0 {
		for _, line := range append(append([]string{}, onlyDirect...), onlyRouted...) {
			t.Errorf("the two routes disagree about a line, and unreportedOnTheWire records no divergence at all; either fix it or record it there with a reason:\n  %s", line)
		}
		return
	}
	if len(onlyDirect) == 0 && len(onlyRouted) == 0 {
		t.Fatal("the two routes printed exactly the same report, which cannot be right while unreportedOnTheWire has entries in it; either the list is stale or this comparison has stopped comparing")
	}

	accounted := map[string]bool{}
	for _, line := range append(append([]string{}, onlyDirect...), onlyRouted...) {
		var matched bool
		for _, field := range unreportedOnTheWire {
			if strings.Contains(line, field) {
				accounted[field] = true
				matched = true
			}
		}
		if !matched {
			t.Errorf("the two routes disagree about a line nothing on unreportedOnTheWire accounts for, which is a divergence somebody has to justify or fix:\n  %s", line)
		}
	}
	for _, field := range unreportedOnTheWire {
		if !accounted[field] {
			t.Errorf("unreportedOnTheWire still lists %q and the two routes printed the same thing about it; if the contract gained it, take it off the list", field)
		}
	}
}

// reportDiff compares two runs of the same command as sets of lines, having
// dropped the two kinds of line that legitimately differ.
//
// The mode announcement differs by construction: saying which route was
// taken is the whole point of it. The JSON startup events carry a timestamp
// per invocation, so comparing them would compare two clocks.
func reportDiff(t *testing.T, direct, routed string) (onlyDirect, onlyRouted []string) {
	t.Helper()
	left, right := reportLines(direct), reportLines(routed)
	if len(left) == 0 || len(right) == 0 {
		t.Fatalf("one of the two reports has no comparable lines left after normalising, so this comparison is empty\ndirect:\n%s\nrouted:\n%s", direct, routed)
	}
	for _, line := range left {
		if !contains(right, line) {
			onlyDirect = append(onlyDirect, line)
		}
	}
	for _, line := range right {
		if !contains(left, line) {
			onlyRouted = append(onlyRouted, line)
		}
	}
	return onlyDirect, onlyRouted
}

func reportLines(out string) []string {
	var kept []string
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
		case strings.HasPrefix(trimmed, modeLine):
		case strings.HasPrefix(trimmed, "{"):
		default:
			kept = append(kept, trimmed)
		}
	}
	return kept
}

// modeLine is what mode.go and readmode.go both prefix their announcement
// with. Spelled out here rather than imported, because package main cannot
// be imported and because it is an operator-facing string: a rename is
// something a deployment feels, so this has to notice one rather than
// follow it.
const modeLine = "mode: "

// TestAReadBesideAnEngineItCannotReachSaysSoRatherThanAnsweringAsIfNothingWereServing
// is the containerised operator's own default, which is the case #545's
// first proof does NOT cover.
//
// Nothing in container/compose.yaml sets $BACKUP_MANAGER_API_URL, so on a
// shipped deployment today every read finds a serving engine, has no route
// to it, and answers from the file. #544 chose to answer rather than refuse,
// for a reason worth repeating: a write that cannot reach the engine has an
// alternative, which is not writing, and a read has none. Taking `status`
// away from every operator whose deployment has not been told where its own
// engine is, at the moment something is already wrong, would buy nothing an
// announcement does not.
//
// So what is asserted is the announcement, in both directions. The answer
// still comes, and it does not claim to be about a world nobody checked:
// "mode: direct. No process has announced itself as serving this
// deployment" is a false sentence here, and a read that printed it would be
// #535 with a reassuring line on top.
func TestAReadBesideAnEngineItCannotReachSaysSoRatherThanAnsweringAsIfNothingWereServing(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the real CLI")
	}
	bin := buildCLI(t, repoRoot(t))

	var reads int
	for _, s := range surfaces {
		// A read is a row that names a mode, writes nothing, and does not
		// go through the write door. The third clause arrived with #636
		// and is not a loosening: a command on that door is REFUSED beside
		// an engine it cannot reach, which is the opposite of what every
		// assertion in this loop asks for, and `medium preflight` is on it
		// now because a check that passes clears a mark. Without the
		// clause it fell in here for the one reason this table cannot see
		// on its own, which is that the invocation it drives names a
		// destination nothing declares and therefore writes nothing.
		if s.mode == "" || s.writes || s.routed {
			continue
		}
		reads++
		t.Run(s.verb+" "+s.name, func(t *testing.T) {
			f := newFixture(t, bin, s)
			release, err := service.AnnounceServing(f.configPath)
			if err != nil {
				t.Fatalf("announcing this process as serving %s: %v", f.configPath, err)
			}
			defer func() { _ = release() }()

			got := runCLI(t, bin, nil, s.argv(f)...)
			if got.code != s.exit {
				t.Fatalf("a read beside an engine it cannot reach exited %d, want %d; it is meant to answer rather than refuse\n%s", got.code, s.exit, got)
			}
			if strings.TrimSpace(got.stdout) == "" {
				t.Errorf("the read answered with nothing at all, so the whole point of not refusing was lost:\n%s", got)
			}
			if !strings.Contains(got.stderr, "mode: unconfirmed") {
				t.Errorf("the read did not say its answer had not been checked against the process serving this deployment:\n%s", got)
			}
			if strings.Contains(got.stderr, "mode: direct") {
				t.Errorf("the read said nothing had announced itself as serving this deployment, and something had:\n%s", got)
			}
		})
	}
	if reads != 5 {
		t.Fatalf("%d rows are recorded as reads that name a mode; #544 gave four surfaces one and #598 made activity the fifth, and a table that has lost or gained one is describing a different product", reads)
	}
}

// TestARoutedWriteRefusesWhenTheEngineServesADifferentDeployment is
// issue #555, and it is the test that used to record the gap rather than
// close it.
//
// The gap was the sharpest one #536 left. A read compares the engine's own
// config_revision against the one this command computed, so a read aimed
// at the wrong engine refuses (that is
// TestTheTwoRoutesCannotDisagreeAboutTheConfiguration's second case). A
// WRITE compared nothing, so $BACKUP_MANAGER_API_URL was taken as naming
// this deployment's engine and one character wrong in a port named
// somebody else's. The write landed there, this deployment's own
// configuration file was untouched, and both surfaces reported success.
//
// What closes it is not the revision. A revision is a hash of
// configuration content, so a staging and a production instance built from
// one template share one, and those two are exactly the pair an operator
// points the wrong address at. What is compared instead is a deployment
// IDENTITY, minted once beside each deployment's journal and served on
// GET /system/version, which is a fact about the instance rather than
// about what it currently holds.
//
// Two deployments, a create typed at the first with the second's address
// in the environment, and the assertion is now that nothing happens
// anywhere and the operator is told which two deployments were involved.
func TestARoutedWriteRefusesWhenTheEngineServesADifferentDeployment(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the real CLI against two real engines")
	}
	bin := buildCLI(t, repoRoot(t))

	_, mine := writeFixture(t)
	_, theirs := writeFixture(t)
	if readFile(t, mine) == readFile(t, theirs) {
		t.Fatal("the two fixtures are byte-identical, so this test could not tell which deployment a write landed in")
	}
	// Both deployments are served, which is the shape this is about: one
	// host running two of these, each with its own engine. Without an
	// engine on the near side the command would be in direct mode and
	// would never look at the route at all, which is how the first draft
	// of this test managed to prove nothing.
	home := startStack(t, mine)
	elsewhere := startStack(t, theirs)
	near := signIn(t, home.uiURL)
	view := signIn(t, elsewhere.uiURL)

	mineID, theirsID := near.deploymentID(), view.deploymentID()
	if mineID == theirsID {
		t.Fatalf("both deployments report the identity %s, so nothing here could tell them apart", mineID)
	}

	mineBefore := readFile(t, mine)
	nearBefore := near.backupSets()
	theirsBefore := view.backupSets()

	// The address of the OTHER deployment's engine, which is what a
	// mistyped port looks like from here.
	got := runCLI(t, bin, routeTo(elsewhere.engineURL), createArgs(mine, writePrivateKey(t), newSetID)...)
	if got.code == 0 {
		t.Fatalf("a create typed at %s was accepted with the engine serving %s in $BACKUP_MANAGER_API_URL, so it went into a deployment nobody was looking at:\n%s", mine, theirs, got)
	}

	// Both identities, because an operator who has just mistyped a URL
	// needs to see the one they meant beside the one they reached. A
	// message carrying only the engine's tells them where they ended up
	// and not what they were aiming at.
	if !strings.Contains(got.output(), theirsID) {
		t.Errorf("the refusal does not name the identity %s of the deployment the engine actually serves:\n%s", theirsID, got)
	}
	if !strings.Contains(got.output(), mineID) {
		t.Errorf("the refusal does not name the identity %s of the deployment this command was typed at:\n%s", mineID, got)
	}
	// The revision check is the read path's and it is a different fact.
	// If this refusal came from there, the guard would still be missing on
	// the case the two configurations are identical, which is the one a
	// staging and a production instance from one template land in.
	if strings.Contains(got.output(), "holding a different configuration") {
		t.Errorf("the write refused on a configuration comparison rather than on which deployment it had reached:\n%s", got)
	}

	if after := readFile(t, mine); after != mineBefore {
		t.Error("the deployment the operator typed this at changed on disk, which no refused write should do")
	}
	if after := near.backupSets(); !equal(after, nearBefore) {
		t.Errorf("the near deployment's own engine took the change after all\nbefore: %v\nafter:  %v", nearBefore, after)
	}
	if after := view.backupSets(); contains(after, newSetID) {
		t.Fatalf("the create was refused and landed in the other deployment anyway\ntheirs before: %v\ntheirs after:  %v\n%s", theirsBefore, after, got)
	}
}

// TestARoutedWriteIsAcceptedByTheDeploymentItWasTypedAt is the control the
// test above needs, and it is the reason that one can fail.
//
// A guard that refused every routed write would satisfy every assertion up
// there and would have taken engine-attached mode away entirely. This is
// the same two-deployment arrangement with the address an operator meant,
// so the same command has to go through.
func TestARoutedWriteIsAcceptedByTheDeploymentItWasTypedAt(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the real CLI against two real engines")
	}
	bin := buildCLI(t, repoRoot(t))

	_, mine := writeFixture(t)
	_, theirs := writeFixture(t)
	home := startStack(t, mine)
	elsewhere := startStack(t, theirs)
	near := signIn(t, home.uiURL)
	view := signIn(t, elsewhere.uiURL)

	theirsBefore := view.backupSets()

	got := runCLI(t, bin, routeTo(home.engineURL), createArgs(mine, writePrivateKey(t), newSetID)...)
	if got.code != 0 {
		t.Fatalf("a create typed at %s with that deployment's own engine in $BACKUP_MANAGER_API_URL was refused:\n%s", mine, got)
	}
	if !contains(near.backupSets(), newSetID) {
		t.Fatalf("the create was accepted and the deployment it was typed at does not have %s:\n%s", newSetID, got)
	}
	if after := view.backupSets(); !equal(after, theirsBefore) {
		t.Errorf("the other deployment on this host moved on a write aimed somewhere else\nbefore: %v\nafter:  %v", theirsBefore, after)
	}
}

// Issue #571: the one shape EPIC #536 documented and did not close, driven
// rather than reasoned about.
//
// An install that has never been configured serves the first-run setup flow
// (#176), and until #571 it announced nothing, because `AnnounceServing`
// reads the journal out of a configuration that is not there yet. So the
// probe every configuration write in the CLI makes had nothing to find, a
// `backup-set create` took the first-configuration path, wrote config.yaml,
// exited 0 and printed the set, and the engine went on serving setup and
// answering 503 until somebody restarted it. That is #535 in its original
// words, on the ordinary path an operator installing fresh and configuring
// from the command line walks straight down.
//
// # Why the real provider binary, and not the composition startStack makes
//
// startStack above mirrors the ordering apps/generic's main.go uses for a
// deployment that HAS a configuration, and that is fine there because the
// thing under test is what the CLI does next to an announcement. Here the
// announcement itself is the subject: what is being checked is that the
// process serving the setup flow announces at all, and a test that made its
// own announcement would be checking its own copy of main.go. So the engine
// below is the real `backupd-web serve`, started as a subprocess
// against a directory with no configuration in it, exactly as the container
// starts it.

// buildWeb builds the provider binary the container image runs as its
// engine. It is the counterpart of buildCLI, and it exists for the reason
// above: the first-run announcement is main.go's to make.
func buildWeb(t *testing.T, root string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "backupd-web")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/backupd-web")
	cmd.Dir = filepath.Join(root, "apps", "generic")
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build backupd-web: %v\n%s", err, out)
	}
	return bin
}

// firstRunStack is a deployment in the state an installer leaves behind:
// no config.yaml at all, an administrator already enrolled, and the real
// engine up and serving the setup flow.
type firstRunStack struct {
	configPath    string
	stateDatabase string
	engineURL     string

	// log is everything the engine has printed so far, so a failure here
	// carries the process's own account of what it did rather than only
	// the CLI's.
	log func() string
}

// startFirstRunStack performs steps 1 and 2 of #571: install, then enrol an
// administrator through the Web UI. Nothing writes a configuration, which
// is #176's whole point and the state the rest of this test is about.
func startFirstRunStack(t *testing.T, webBin string) *firstRunStack {
	t.Helper()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	// The container's own shape: the state directory is a bind mount, so it
	// exists before anything starts, and the local-auth store sits inside it
	// (container/compose.yaml mounts $STATE_DIR at /data/state and
	// apps/common/auth/local keeps local-auth.json there).
	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(%s): %v", stateDir, err)
	}
	stateDatabase := filepath.Join(stateDir, "state.db")
	storePath := filepath.Join(stateDir, "local-auth.json")

	// Step 2, done the way `backupd-web auth create-admin` does it,
	// because this test needs a password to sign in with. An operator
	// redeems the bootstrap token instead and ends up with the same record.
	if _, err := local.CreateAdmin(local.CreateAdminConfig{
		StorePath: storePath,
		Username:  testAdmin,
		Password:  testPassword,
	}); err != nil {
		t.Fatalf("CreateAdmin: %v", err)
	}

	addr := freeAddr(t)
	out := &syncBuffer{}
	cmd := exec.Command(webBin, "serve",
		"--config", configPath,
		"--state-database", stateDatabase,
		"--auth-store", storePath,
		"--listen", addr,
		"--profile", "generic",
	)
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting backupd-web: %v", err)
	}
	t.Cleanup(func() {
		if err := cmd.Process.Signal(os.Interrupt); err != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	})

	base := "http://" + addr
	waitForLive(t, base, out)
	return &firstRunStack{
		configPath:    configPath,
		stateDatabase: stateDatabase,
		engineURL:     base,
		log:           out.String,
	}
}

// freeAddr picks a loopback address nothing is listening on. The engine
// takes an address rather than handing one back, so the port has to be
// chosen out here.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("releasing the reserved port: %v", err)
	}
	return addr
}

// syncBuffer collects a subprocess's output while the test reads it. The
// child writes from its own goroutine, so an ordinary bytes.Buffer here is
// a data race the race detector would (rightly) fail the suite over.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitForLive blocks until the engine answers its liveness probe, which is
// unauthenticated and is served by a first-run instance exactly as it is by
// a configured one.
func waitForLive(t *testing.T, base string, log *syncBuffer) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/health/live")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the engine at %s never answered /health/live:\n%s", base, log.String())
}

// notReady reports whether the engine is answering the 503 an instance with
// no backend answers. It is the fact #571 opens with, and it is read
// without credentials because that is what the probe is.
func notReady(t *testing.T, base string) bool {
	t.Helper()
	resp, err := http.Get(base + "/health/ready")
	if err != nil {
		t.Fatalf("GET %s/health/ready: %v", base, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusServiceUnavailable
}

// backupSetsOrUnconfigured is backupSets for a deployment that may have no
// configuration at all.
//
// A first-run instance serves a route table with no /backup-sets on it and
// answers 503 NOT_CONFIGURED to everything else (apps/common/webhost's
// newUnconfiguredRouter), so "this engine has no world yet" comes back as an
// empty list rather than as a fatal. That is what makes the two routes
// comparable across the moment a deployment is configured, which is the
// moment #571 is about.
func (b *browser) backupSetsOrUnconfigured() []string {
	b.t.Helper()
	resp, err := b.client.Get(b.base + "/api/v1/backup-sets")
	if err != nil {
		b.t.Fatalf("GET /api/v1/backup-sets: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusServiceUnavailable {
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		b.t.Fatalf("GET /api/v1/backup-sets returned %d: %s", resp.StatusCode, raw)
	}
	var answer apicontract.ListBackupSetsResponse
	if err := json.Unmarshal(raw, &answer); err != nil {
		b.t.Fatalf("decoding /api/v1/backup-sets: %v\n%s", err, raw)
	}
	ids := make([]string, 0, len(answer.BackupSets))
	for _, bs := range answer.BackupSets {
		ids = append(ids, bs.ID)
	}
	sort.Strings(ids)
	return ids
}

// cliBackupSets is the same question asked of the other route: the backup
// sets `sources` prints.
//
// A configuration that is not there is an empty world rather than an error
// to report, which is what lets this be compared against the engine's answer
// on a deployment that has never been configured. A `sources` that refuses
// for any OTHER reason is a fatal, so an empty answer here always means
// "nothing is configured" and never "the read fell over".
func cliBackupSets(t *testing.T, bin, configPath string) []string {
	t.Helper()
	got := runCLI(t, bin, nil, "sources", "--config", configPath)
	if got.code != 0 {
		if _, err := os.Stat(configPath); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		t.Fatalf("`sources` refused on a deployment that has a configuration:\n%s", got)
	}
	var ids []string
	for _, line := range strings.Split(got.stdout, "\n") {
		if !strings.HasPrefix(line, "  ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.Contains(fields[0], "/") {
			continue
		}
		ids = append(ids, fields[0])
	}
	sort.Strings(ids)
	return ids
}

// exitEngineHoldsDeployment is the code the binary's own usage block
// documents for a write refused because something else is serving this
// deployment (#551). Spelled out here rather than imported, because package
// main cannot be imported and because the code is a contract a script reads.
const exitEngineHoldsDeployment = 3

// TestAFirstCreateFromTheCLICannotLandBehindTheSetupFlow is issue #571.
//
// The property is not "a refusal appeared". It is that after a CLI create,
// no surface reports a world another surface does not have: the engine's own
// list and the CLI's own list have to be the same list, whichever way the
// command went. A guard that refused and left the file written would satisfy
// an exit-code assertion and fail this one.
//
// The second case is the control that keeps the first from being satisfied
// by refusing everything. A genuinely bare host, with nothing serving and no
// state directory yet, still writes its first configuration from the command
// line, which is the case the direct path exists for and the one the
// installer's own worked examples show.
func TestAFirstCreateFromTheCLICannotLandBehindTheSetupFlow(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the real CLI against the real first-run engine")
	}
	root := repoRoot(t)
	bin := buildCLI(t, root)

	t.Run("beside an engine serving the setup flow", func(t *testing.T) {
		live := startFirstRunStack(t, buildWeb(t, root))
		view := signIn(t, live.engineURL)

		// The state #571 was reported from, asserted rather than assumed.
		// Without these three the case below could pass on a deployment
		// that was already configured, which is a different issue with a
		// different answer.
		if _, err := os.Stat(live.configPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("this deployment already has a configuration at %s (stat err = %v), so it is not the fresh install #571 is about", live.configPath, err)
		}
		if !notReady(t, live.engineURL) {
			t.Fatalf("the engine is ready before anything has configured it, so it is not serving the setup flow:\n%s", live.log())
		}
		if sets := view.backupSetsOrUnconfigured(); len(sets) != 0 {
			t.Fatalf("the engine already lists backup sets on a deployment with no configuration: %v", sets)
		}

		got := runCLI(t, bin, nil, createArgs(live.configPath, writePrivateKey(t), newSetID,
			"--state-database", live.stateDatabase)...)

		engineSets := view.backupSetsOrUnconfigured()
		cliSets := cliBackupSets(t, bin, live.configPath)
		if !equal(cliSets, engineSets) {
			t.Fatalf("the CLI and the engine serving this deployment describe different worlds after a `backup-set create`, which is issue #535 on a fresh install.\nCLI:    %v\nengine: %v\n%s\nengine log:\n%s",
				cliSets, engineSets, got, live.log())
		}

		// And the refusal is the one #538, #542 and #551 already promise
		// for the second create, so a fresh install is not a deployment
		// with refusals of its own shape.
		if got.code != exitEngineHoldsDeployment {
			t.Errorf("the create exited %d, want %d so a script can tell this apart from an ordinary failure:\n%s", got.code, exitEngineHoldsDeployment, got)
		}
		if !strings.Contains(got.output(), "mode: engine-attached") {
			t.Errorf("the create did not announce which world it believed it was in:\n%s", got)
		}
		if !strings.Contains(got.output(), "refused here rather than downgraded") {
			t.Errorf("the refusal did not say that nothing was written:\n%s", got)
		}
		if _, err := os.Stat(live.configPath); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a refused create wrote %s underneath the engine serving the setup flow (stat err = %v)", live.configPath, err)
		}
		// The setup flow is still there, which is the remedy the operator
		// actually has: an instance torn down by this refusal would have
		// traded one silent divergence for an install nobody can finish.
		if !notReady(t, live.engineURL) {
			t.Errorf("the engine stopped serving the setup flow after a refused create:\n%s", live.log())
		}
	})

	t.Run("with nothing serving, a bare host still writes its first configuration", func(t *testing.T) {
		dir := t.TempDir()
		configPath := filepath.Join(dir, "config.yaml")
		// Deliberately a directory that does not exist. A first-ever start
		// against a fresh volume is exactly this, and a fix that needed the
		// state directory to be there already would refuse the one case the
		// first-configuration path exists for.
		stateDatabase := filepath.Join(dir, "state", "state.db")

		got := runCLI(t, bin, nil, createArgs(configPath, writePrivateKey(t), newSetID,
			"--state-database", stateDatabase)...)
		if got.code != 0 {
			t.Fatalf("a first create on a host with nothing serving was refused:\n%s", got)
		}
		if !strings.Contains(got.output(), "mode: direct") {
			t.Errorf("the create did not announce direct mode on a deployment nothing is serving:\n%s", got)
		}
		if _, err := os.Stat(configPath); err != nil {
			t.Fatalf("the create reported success and wrote no configuration to %s: %v\n%s", configPath, err, got)
		}
		if sets := cliBackupSets(t, bin, configPath); !contains(sets, newSetID) {
			t.Errorf("the first configuration does not hold the set the create reported: %v\n%s", sets, got)
		}
	})
}

// post sends one authenticated, CSRF-carrying JSON request, exactly the
// way the Web UI's own fetch does, and hands back the status and the raw
// body so a caller can decode or complain about whichever it gets.
func (b *browser) post(path string, payload any) (int, []byte) {
	b.t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		b.t.Fatalf("encoding a %s body: %v", path, err)
	}
	req, err := http.NewRequest(http.MethodPost, b.base+path, bytes.NewReader(raw))
	if err != nil {
		b.t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(local.CSRFHeaderName, b.csrf())
	resp, err := b.client.Do(req)
	if err != nil {
		b.t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// importSSHKey is the wizard's first write: key material in, a reference
// back, and nothing but the reference used afterwards.
func (b *browser) importSSHKey(keyPath string) string {
	b.t.Helper()
	pem, err := os.ReadFile(keyPath)
	if err != nil {
		b.t.Fatalf("ReadFile(%s): %v", keyPath, err)
	}
	code, body := b.post("/api/v1/ssh-keys", apicontract.ImportSSHKeyRequest{PrivateKeyPEM: string(pem)})
	if code != http.StatusCreated && code != http.StatusOK {
		b.t.Fatalf("importing a key into the setup flow returned %d: %s", code, body)
	}
	var answer apicontract.ImportSSHKeyResponse
	if err := json.Unmarshal(body, &answer); err != nil {
		b.t.Fatalf("decoding the imported key: %v\n%s", err, body)
	}
	if answer.ID == "" {
		b.t.Fatalf("the setup flow imported a key and named no id: %s", body)
	}
	return answer.ID
}

// TestTheWizardsReadOnlyChoiceSurvivesTheFirstRunSave drives the setup
// flow the way a browser does and reads the result back off the engine.
//
// `completeFirstRun` assembles its service.CreateBackupSetRequest field by
// field and dropped `read_only`, so an operator who ticked "read only" in
// the wizard on a fresh install got a set that was not read only, with
// nothing anywhere saying so. Read-only is the declaration that stops this
// manager ever deleting the remote copies (#282, #316) and the wizard
// offers it as a safety choice, so a silent discard leaves somebody
// believing they asked for the safer posture when they did not.
//
// The exhaustive guard is apps/common/webhost's
// TestCompleteFirstRun_CarriesEveryFieldOfTheSpecItWasGiven, which walks
// backupSetSpec itself and fails on any field with no row, because a
// hand-built request that omits one field is a shape that omits more. What
// this adds is the chain that guard cannot reach: the real binary, the real
// router, a real configuration written to disk, activation in the same
// process, and the answer the Web UI would then render. That is the level
// the discrepancy was found at, so it is a level worth being able to fail
// at.
func TestTheWizardsReadOnlyChoiceSurvivesTheFirstRunSave(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the real first-run engine")
	}
	live := startFirstRunStack(t, buildWeb(t, repoRoot(t)))
	view := signIn(t, live.engineURL)

	spec := apicontract.BackupSetSpec{
		SourceName:         "api",
		Name:               "var-backups",
		Host:               "source.example.internal",
		Port:               2222,
		User:               "backupuser",
		SSHKeyID:           view.importSSHKey(writePrivateKey(t)),
		KnownHostsLine:     aKnownHostsLine,
		RemotePath:         "/srv/backups",
		LocalPath:          filepath.Join(t.TempDir(), "backups"),
		Include:            []string{"*.dump"},
		CompletionStrategy: "rename",
		StaleAfterSeconds:  172800,
		// The tick under test. Everything above it is here so the save is
		// a real one rather than a minimal one.
		ReadOnly: true,
		// Issue #624: the service proves a first set's connection in front
		// of the write, and source.example.internal is a name that
		// resolves nowhere. This case is about the read-only tick surviving
		// a real save, activation and a read back, not about the check, so
		// it skips it the way every `backup-set create` in this package
		// does (createArgs). The check's own cases are in core/service and
		// apps/common/webhost, and its real-machine proof is
		// scripts/e2e/two-machine-backup.sh.
		SkipConnectionCheck: true,
	}

	code, body := view.post("/api/v1/system/first-run", spec)
	if code != http.StatusCreated {
		t.Fatalf("the setup flow refused a valid submission with %d: %s\nengine log:\n%s", code, body, live.log())
	}
	var saved apicontract.CompleteFirstRunResponse
	if err := json.Unmarshal(body, &saved); err != nil {
		t.Fatalf("decoding the setup response: %v\n%s", err, body)
	}
	if saved.RestartRequired {
		t.Fatalf("the instance could not activate against the configuration it just wrote, so what it serves below is not what this test is about:\n%s", live.log())
	}
	if !saved.BackupSet.ReadOnly {
		t.Errorf("the wizard's own 201 says the set it just created is not read-only, though that is what was asked for:\n%s", body)
	}

	// And what the Web UI would render, off the engine this process is now
	// serving, with nothing restarted.
	sets := view.backupSetsOrUnconfigured()
	if !contains(sets, "api/var-backups") {
		t.Fatalf("the setup flow reported success and the engine does not serve the set: %v\nengine log:\n%s", sets, live.log())
	}
	served := view.backupSet(t, "api/var-backups")
	if !served.ReadOnly {
		t.Errorf("an operator ticked read-only in the wizard and the engine serves a set that will delete remote copies. That declaration is the whole of what read-only is for, and nothing told them it had been dropped.\nasked for: %+v\nserving:   %+v", spec, served)
	}
	// A few fields beside it, so a fix that hard-coded read_only true and
	// dropped something else would not pass here either.
	if served.RemotePath != spec.RemotePath || served.Port != spec.Port || served.CompletionStrategy != spec.CompletionStrategy || served.StaleAfterSeconds != spec.StaleAfterSeconds {
		t.Errorf("the engine is serving a set that differs from the one the wizard submitted\nasked for: %+v\nserving:   %+v", spec, served)
	}
}

// backupSet reads one set out of what the engine serves, by id.
func (b *browser) backupSet(t *testing.T, id string) apicontract.BackupSet {
	t.Helper()
	var answer apicontract.ListBackupSetsResponse
	b.get("/api/v1/backup-sets", &answer)
	for _, bs := range answer.BackupSets {
		if bs.ID == id {
			return bs
		}
	}
	t.Fatalf("the engine serves no backup set %s", id)
	return apicontract.BackupSet{}
}
