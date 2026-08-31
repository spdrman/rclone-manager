package webhost

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/apps/common/csrf"
	"github.com/spdrman/rclone-manager/core/service"
)

// settings_gate_test.go is issue #87 (B5.1)'s adversarial reading of
// issue #171's decision that PATCH /api/v1/settings is deliberately NOT
// behind the destructive gate.
//
// # The decision is right and the reason given for it is not
//
// #171 argues that turning protect_last_known_good off only widens what a
// LATER retention apply may delete, and that apply is gated, so "the gate
// still stands between this deployment and every deletion". That sentence
// is true only while the gate is false, which is to say only while the
// product does not work. DestructiveGate is a static, deployment-wide
// attestation (see gate.go): #92 flips it to true once, permanently, for
// every request thereafter. From that moment the gate stands between
// nothing and nothing, and an argument that rests on it has stopped
// proving anything about a shipped deployment.
//
// What actually protects the apply, before and after #92, is ONE thing
// the gate has nothing to do with: a retention plan is bound to the
// configuration revision it was computed against, so ANY settings write
// in between makes the plan the operator approved stale, and the apply is
// refused by name.
//
// So the settings route cannot widen an ALREADY-APPROVED deletion, which
// is #171's conclusion, reached by a route that survives #92 landing.
// Obtaining a wider plan after the write is one more call, and that call
// is a fresh preview with the widened DELETE list in its body.
//
// # What this file does NOT claim, and used to
//
// The version of this argument written for #87 had a second limb: "the
// plan the operator sees in the preview after that write is the one
// computed under the new policy, with the widened DELETE list visible in
// it". That sentence is true of the shipped UI flow and is not a property
// of the API (issue #87's review, M6). ApplyRetentionRequest carries only
// plan_id; nothing binds an apply to a preview a human looked at, and an
// API caller can PATCH /settings, GET the preview and POST the apply with
// no human anywhere in the sequence. Stating it as though it were part of
// the HTTP contract would over-credit this route to whoever tiers the
// next mutating one, so router.go now says the narrower, enforced thing
// and names the UI as the place the re-confirmation lives.
//
// The widening itself, protect_last_known_good off putting an artifact no
// GFS tier selects onto the DELETE list, is proved where the decision is
// made: internal/retention's TestApplyLastKnownGoodKeepsArtifactOutsideEveryGFSTier
// and TestDecideKeepReflectsDisabledProtectionEndToEnd. It is not
// reproducible over this fixture, and the reason is worth writing down
// rather than working around: every artifact a real cycle creates here is
// discovered NOW, so the newest one is both the daily tier's
// representative and the last-known-good holder, and turning protection
// off widens nothing. Back-dating an artifact means writing journal rows,
// which is internal to core. What this file CAN observe, and now does, is
// a real deletion.
//
// core/service's own
// TestApplyRetentionPlan_ASettingsWriteBetweenPreviewAndApplyIsStale pins
// the staleness rule a layer down, and
// TestApplyRetentionPlan_ExpiredPlanIsStaleWithZeroDeletions pins the
// expires_at boundary (which needs the package-private TTL, and which
// surfaces here as the same RETENTION_PLAN_STALE code); neither is a
// substitute for this file, because the claim in router.go is about two
// HTTP routes.
//
// # What the settings route could NOT be used for, and why that matters
//
// The adversarial question worth asking is whether any branch of this
// route reaches a deletion that is not itself plan-bound. The one to
// check is the scheduler: it runs a full cycle on its own timer, behind
// no gate and with no human in front of it, and that cycle does delete
// (the remote artifact, after a committed local copy). It never calls
// retention apply, and no field this route can write reaches it -
// UpdateSettingsRequest carries a retention section and nothing else, and
// retention timezone, week start, tier chain and protect_last_known_good
// are read at plan time. If that ever stops being true, a settings write
// becomes a way to cause an unattended deletion and the gate tier of this
// route becomes the wrong question rather than a settled one.

func gatedBoundaryRouter(t *testing.T, configPath string) (http.Handler, func()) {
	t.Helper()
	svc, cleanup, err := service.Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("service.Open: %v", err)
	}
	// alwaysPassGate, deliberately: with the shipped gate this test could
	// not reach the apply handler at all, and a suite that never reaches
	// the code it is reasoning about is the exact failure mode issue #87
	// is here to find.
	router := NewRouter(RouterConfig{
		Platform:      allowingPlatform("alice"),
		Backend:       svc,
		Gate:          alwaysPassGate{},
		BinaryVersion: "test",
		Commit:        "test",
	})
	return router, func() { _ = cleanup() }
}

// call fires one request at the router with a satisfied CSRF pair, so a
// refusal is never the CSRF check answering for something else.
func call(t *testing.T, router http.Handler, method, path, body string) (int, string) {
	t.Helper()
	var r *strings.Reader
	if body != "" {
		r = strings.NewReader(body)
	} else {
		r = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set("Content-Type", "application/json")
	const token = "settings-gate-token"
	req.AddCookie(&http.Cookie{Name: csrf.CookieName, Value: token})
	req.Header.Set(csrf.HeaderName, token)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func planIDOf(t *testing.T, body string) string {
	t.Helper()
	var doc struct {
		PlanID string `json:"plan_id"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("unmarshal plan: %v (body %s)", err, body)
	}
	if doc.PlanID == "" {
		t.Fatalf("the preview carried no plan_id: %s", body)
	}
	return doc.PlanID
}

const (
	previewPath = "/api/v1/backup-sets/production/postgres-primary/retention/preview"
	applyPath   = "/api/v1/backup-sets/production/postgres-primary/retention/apply"
)

// seededBoundaryConfig is writeBoundaryConfig plus a real cycle over TWO
// remote artifacts, so the retention preview below carries a real
// deletion instead of an empty verdict list.
//
// This is issue #87's review, M6, and it is the difference between a
// suite that reasons about deletions and one that observes one. The
// fixture's local_path used to be an empty directory that was never even
// created, so every preview in this file returned
// keep_count 0, delete_count 0, verdicts [], and the positive control
// "applies an empty plan and gets a 200" proved only that the route
// answers.
//
// Two same-day artifacts under the default 7/3/12 chain give exactly one
// KEEP (the daily tier's representative, which is also the
// last-known-good holder) and exactly one DELETE, deterministically.
//
// It returns the config path and the local directory the artifacts landed
// in, because "deletes exactly the approved set and nothing else" is a
// claim about that directory.
func seededBoundaryConfig(t *testing.T) (string, string) {
	t.Helper()
	configPath := writeBoundaryConfig(t)
	// The fixture's own layout, from writeBoundaryConfig
	// (settings_boundary_test.go): remote/ and local/ beside the config.
	dir := filepath.Dir(configPath)
	remoteDir := filepath.Join(dir, "remote")
	localDir := filepath.Join(dir, "local")

	if err := os.WriteFile(filepath.Join(remoteDir, "older.dump"), []byte("a second artifact, so a retention plan has something to select"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	runCLI(t, "run", "--config", configPath)

	return configPath, localDir
}

// artifactsIn lists the managed artifact files in the backup set's local
// directory, ignoring the sidecar manifests, so two listings can be
// compared as sets.
func artifactsIn(t *testing.T, localDir string) []string {
	t.Helper()
	entries, err := os.ReadDir(localDir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", localDir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".manifest.json") {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// verdictsOf reads the preview's own verdict list, split by action.
func verdictsOf(t *testing.T, body string) (keep, del []string) {
	t.Helper()
	var doc struct {
		KeepCount   int `json:"keep_count"`
		DeleteCount int `json:"delete_count"`
		Verdicts    []struct {
			Artifact string `json:"artifact"`
			Action   string `json:"action"`
		} `json:"verdicts"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("unmarshal plan: %v (body %s)", err, body)
	}
	for _, v := range doc.Verdicts {
		switch v.Action {
		case "KEEP":
			keep = append(keep, v.Artifact)
		case "DELETE":
			del = append(del, v.Artifact)
		}
	}
	if len(del) != doc.DeleteCount || len(keep) != doc.KeepCount {
		t.Fatalf("the preview's counts (keep %d, delete %d) disagree with its own verdict list (keep %v, delete %v)", doc.KeepCount, doc.DeleteCount, keep, del)
	}
	sort.Strings(keep)
	sort.Strings(del)
	return keep, del
}

// TestThePreviewThisFileReasonsAboutContainsARealDeletion is the
// non-vacuity guard for everything below it. Every other test in this
// file is about what happens to an approved DELETE list, and each of them
// passes trivially over an empty one.
func TestThePreviewThisFileReasonsAboutContainsARealDeletion(t *testing.T) {
	configPath, localDir := seededBoundaryConfig(t)
	router, closeRouter := gatedBoundaryRouter(t, configPath)
	defer closeRouter()

	code, body := call(t, router, http.MethodGet, previewPath, "")
	if code != http.StatusOK {
		t.Fatalf("preview returned %d: %s", code, body)
	}
	keep, del := verdictsOf(t, body)
	if len(del) != 1 || len(keep) != 1 {
		t.Fatalf("the preview selected %d artifact(s) for deletion and kept %d, want exactly one of each: %s\n"+
			"the fixture no longer produces the two-artifact, one-deletion shape every assertion in this file is written against", len(del), len(keep), body)
	}
	if on := artifactsIn(t, localDir); len(on) != 2 {
		t.Fatalf("the backup set holds %v on disk, want two artifacts; the plan above describes something this test cannot see", on)
	}
}

// TestAnUngatedSettingsWriteCannotApplyAnAlreadyApprovedPlan is the
// assertion #171's reasoning should have rested on. The plan it approves
// carries a real DELETE, and the refusal is asserted to have deleted
// nothing: a stale plan that was refused after the deletion would satisfy
// a status-code assertion just as well.
func TestAnUngatedSettingsWriteCannotApplyAnAlreadyApprovedPlan(t *testing.T) {
	configPath, localDir := seededBoundaryConfig(t)
	router, closeRouter := gatedBoundaryRouter(t, configPath)
	defer closeRouter()

	code, body := call(t, router, http.MethodGet, previewPath, "")
	if code != http.StatusOK {
		t.Fatalf("preview returned %d: %s", code, body)
	}
	approved := planIDOf(t, body)
	_, approvedDeletes := verdictsOf(t, body)
	if len(approvedDeletes) == 0 {
		t.Fatal("the approved plan deletes nothing, so refusing it later proves nothing about widening a deletion")
	}
	before := artifactsIn(t, localDir)

	// The ungated write, doing the most dangerous thing this route can
	// do: turning FR-19's last-known-good protection off.
	code, body = call(t, router, http.MethodPatch, "/api/v1/settings", `{"retention":{"protect_last_known_good":false}}`)
	if code != http.StatusOK {
		t.Fatalf("the settings write returned %d, so this test never set up the situation it is about: %s", code, body)
	}
	if !strings.Contains(body, `"protect_last_known_good":false`) {
		t.Fatalf("the settings write did not actually turn the protection off: %s", body)
	}

	// The plan the operator approved BEFORE that write must now be
	// refused, by name.
	code, body = call(t, router, http.MethodPost, applyPath, `{"plan_id":"`+approved+`"}`)
	if code != http.StatusConflict {
		t.Fatalf("applying a plan approved before the settings write returned %d, want 409: %s\n"+
			"an ungated settings route that can widen a plan already approved under the old policy is a way to delete more than the operator confirmed", code, body)
	}
	if got := responseErrorCode(body); got != "RETENTION_PLAN_STALE" {
		t.Errorf("error code = %q, want RETENTION_PLAN_STALE; any other 409 means the apply was refused for some other reason and this proves nothing about the settings write", got)
	}

	// Refused BEFORE anything was deleted, not after. A 409 carrying the
	// right code would look identical on a build that deleted the
	// artifact and then reported the staleness.
	if after := artifactsIn(t, localDir); !slices.Equal(before, after) {
		t.Errorf("the backup set held %v before the refused apply and %v after; the plan the operator approved under the old policy was carried out anyway", before, after)
	}
}

// TestAPlanApprovedWithNoSettingsWriteInBetweenStillApplies is the
// positive control. Without it the assertion above passes just as well on
// a build where retention apply is broken outright.
//
// It also carries the "and nothing else" half of the claim: the apply
// removes exactly the artifacts the operator's own preview named for
// deletion, and leaves every artifact it named as kept.
func TestAPlanApprovedWithNoSettingsWriteInBetweenStillApplies(t *testing.T) {
	configPath, localDir := seededBoundaryConfig(t)
	router, closeRouter := gatedBoundaryRouter(t, configPath)
	defer closeRouter()

	code, body := call(t, router, http.MethodGet, previewPath, "")
	if code != http.StatusOK {
		t.Fatalf("preview returned %d: %s", code, body)
	}
	approved := planIDOf(t, body)
	keep, del := verdictsOf(t, body)
	if len(del) == 0 {
		t.Fatal("the plan deletes nothing, so applying it proves only that the route answers 200")
	}
	before := artifactsIn(t, localDir)

	code, body = call(t, router, http.MethodPost, applyPath, `{"plan_id":"`+approved+`"}`)
	if code != http.StatusOK {
		t.Fatalf("applying an untouched plan returned %d, want 200: %s", code, body)
	}

	after := artifactsIn(t, localDir)
	for _, name := range del {
		if slices.Contains(after, name) {
			t.Errorf("%s was on the approved DELETE list and is still on disk after the apply", name)
		}
		if !slices.Contains(before, name) {
			t.Errorf("%s was on the approved DELETE list but was never on disk, so its absence afterwards proves nothing", name)
		}
	}
	for _, name := range keep {
		if !slices.Contains(after, name) {
			t.Errorf("%s was on the approved KEEP list and the apply deleted it anyway; an apply that deletes more than the operator confirmed is the failure this whole boundary exists to prevent", name)
		}
	}
	if len(after) != len(before)-len(del) {
		t.Errorf("the backup set held %v before the apply and %v after, which is not %d artifact(s) removed", before, after, len(del))
	}
}

// settingsWriteSurface is every leaf field a settings write can carry,
// as a dotted path, with the reason each one is allowed to be there.
//
// A map with a reason per entry rather than a list, matching
// declaredAbsentPaths and TestProfileCarriesNoBackupDomainPolicy
// elsewhere in this repository: an entry cannot exist without somebody
// having written down why the scheduler is still unattended-safe with it
// present. Adding a field to this surface means adding a line here, and
// the line is where the gate-tier question gets re-answered.
var settingsWriteSurface = map[string]string{
	"Retention.Timezone":             "names the calendar a retention plan's buckets are computed in. Read at plan time, by a plan the operator previews and applies; the scheduler's own cycle never calls retention apply.",
	"Retention.WeekStartsOn":         "same as Timezone: a bucket-boundary input read at plan time.",
	"Retention.Tiers.Name":           "a tier label. config.Validate refuses the one reserved name (last_known_good) and holds the rest to an anchored pattern.",
	"Retention.Tiers.Granularity":    "a closed value set (config.RetentionGranularities), read at plan time.",
	"Retention.Tiers.PeriodDays":     "a bounded look-back, read at plan time.",
	"Retention.Tiers.Keep":           "a bounded count, read at plan time.",
	"Retention.Tiers.WindowUnit":     "a closed value set, read at plan time.",
	"Retention.ProtectLastKnownGood": "FR-19's protection. The dangerous one, and the reason #171 was asked in the first place: turning it off WIDENS what a later retention apply may delete. That apply is plan-bound (see this file's doc comment), and the scheduler never performs one.",
}

// TestTheSettingsWriteSurfaceReachesNothingButRetention is the structural
// half of the argument above: #171's conclusion holds because a settings
// write cannot reach anything the scheduler acts on unattended.
//
// It walks the request type RECURSIVELY (issue #87's review, M7). The
// previous version compared rt.NumField() against ["Retention"], and its
// comment claimed "if a future section is added to this request type,
// this fails". UpdateSettingsRequest already had exactly one field, so
// the natural place to add a knob was INSIDE it: Retention.Schedule or
// Retention.Concurrency would have changed nothing that assertion could
// see. The guard was one level shallower than the claim router.go quotes
// it for, which is this branch's own defect class.
func TestTheSettingsWriteSurfaceReachesNothingButRetention(t *testing.T) {
	got := leafFieldPaths(t, reflect.TypeOf(service.UpdateSettingsRequest{}))
	if len(got) == 0 {
		t.Fatal("the walk found no fields at all, so it cannot notice one being added")
	}

	for _, path := range got {
		if _, allowed := settingsWriteSurface[path]; !allowed {
			t.Errorf("a settings write can now carry %q, which is not in settingsWriteSurface.\n"+
				"#171's argument that this route cannot cause an unattended deletion was made about a retention-only write surface, and it has to be re-made for this field: say what the scheduler does with it, and add the entry with that reason.", path)
		}
	}
	for path := range settingsWriteSurface {
		if !slices.Contains(got, path) {
			t.Errorf("settingsWriteSurface names %q, which the request type no longer carries; a stale allow-list entry hides the next real addition", path)
		}
	}
}

// TestTheWriteSurfaceWalkSeesNestedFields is the positive control for the
// walk itself. A recursive walk that quietly stopped at the top level
// would leave the test above asserting exactly what the shallow version
// asserted, and passing just as green.
func TestTheWriteSurfaceWalkSeesNestedFields(t *testing.T) {
	type inner struct {
		Deep    string
		Deeper  *string
		Repeats []struct{ Leaf int }
	}
	type outer struct {
		Section  *inner
		Sections map[string]inner
		hidden   int //nolint:unused // deliberately unexported: the walk must skip it
	}

	got := leafFieldPaths(t, reflect.TypeOf(outer{}))
	for _, want := range []string{
		"Section.Deep",
		"Section.Deeper",
		"Section.Repeats.Leaf",
		"Sections.Deep",
		"Sections.Deeper",
		"Sections.Repeats.Leaf",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("the walk missed %q; it reported %v", want, got)
		}
	}
	for _, never := range got {
		if strings.Contains(never, "hidden") {
			t.Errorf("the walk reported the unexported field %q, which no JSON body can set", never)
		}
	}
}

// leafFieldPaths lists every exported LEAF field of a struct type as a
// dotted path, descending through pointers, slices, arrays and map
// values. A leaf is anything that is not itself a struct after those
// indirections are followed, which is exactly the set of places a JSON
// body can put a value.
//
// It returns an error rather than panicking on a non-struct, which the
// shallow version did not: unreachable today is not a reason to leave a
// panic where a failure belongs.
func leafFieldPaths(t *testing.T, rt reflect.Type) []string {
	t.Helper()
	var out []string
	var walk func(rt reflect.Type, prefix string, depth int)
	walk = func(rt reflect.Type, prefix string, depth int) {
		if depth > 16 {
			t.Fatalf("the type walk hit depth %d at %q, which means a recursive type; the allow-list cannot describe one", depth, prefix)
		}
		rt = elemOf(rt)
		if rt.Kind() != reflect.Struct {
			if prefix != "" {
				out = append(out, prefix)
			}
			return
		}
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			if !f.IsExported() {
				continue
			}
			path := f.Name
			if prefix != "" {
				path = prefix + "." + f.Name
			}
			ft := elemOf(f.Type)
			if ft.Kind() == reflect.Struct {
				walk(ft, path, depth+1)
				continue
			}
			out = append(out, path)
		}
	}
	if elemOf(rt).Kind() != reflect.Struct {
		t.Fatalf("leafFieldPaths needs a struct type, got %s", rt.Kind())
	}
	walk(rt, "", 0)
	sort.Strings(out)
	return out
}

// elemOf follows pointers, slices, arrays and map values down to the type
// a value actually lands in.
func elemOf(rt reflect.Type) reflect.Type {
	for {
		switch rt.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
			rt = rt.Elem()
		default:
			return rt
		}
	}
}
