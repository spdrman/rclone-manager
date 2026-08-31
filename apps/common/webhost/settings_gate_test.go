package webhost

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
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
// What actually protects the apply, before and after #92, is two things
// the gate has nothing to do with:
//
//  1. a retention plan is bound to the configuration revision it was
//     computed against, so ANY settings write in between makes the plan
//     the operator approved stale, and
//  2. the plan the operator sees in the preview after that write is the
//     one computed under the new policy, with the widened DELETE list
//     visible in it.
//
// So the settings route cannot widen an already-approved deletion, which
// is #171's conclusion, reached by a route that survives #92 landing.
// This file pins that mechanism at the HTTP boundary, where the argument
// is actually made. core/service's own
// TestApplyRetentionPlan_ASettingsWriteBetweenPreviewAndApplyIsStale
// pins the same rule a layer down; neither is a substitute for the other,
// because the claim in router.go is about two HTTP routes.
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

// TestAnUngatedSettingsWriteCannotApplyAnAlreadyApprovedPlan is the
// assertion #171's reasoning should have rested on.
func TestAnUngatedSettingsWriteCannotApplyAnAlreadyApprovedPlan(t *testing.T) {
	configPath := writeBoundaryConfig(t)
	router, closeRouter := gatedBoundaryRouter(t, configPath)
	defer closeRouter()

	code, body := call(t, router, http.MethodGet, previewPath, "")
	if code != http.StatusOK {
		t.Fatalf("preview returned %d: %s", code, body)
	}
	approved := planIDOf(t, body)

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
}

// TestAPlanApprovedWithNoSettingsWriteInBetweenStillApplies is the
// positive control. Without it the assertion above passes just as well on
// a build where retention apply is broken outright.
func TestAPlanApprovedWithNoSettingsWriteInBetweenStillApplies(t *testing.T) {
	configPath := writeBoundaryConfig(t)
	router, closeRouter := gatedBoundaryRouter(t, configPath)
	defer closeRouter()

	code, body := call(t, router, http.MethodGet, previewPath, "")
	if code != http.StatusOK {
		t.Fatalf("preview returned %d: %s", code, body)
	}
	approved := planIDOf(t, body)

	code, body = call(t, router, http.MethodPost, applyPath, `{"plan_id":"`+approved+`"}`)
	if code != http.StatusOK {
		t.Fatalf("applying an untouched plan returned %d, want 200: %s", code, body)
	}
}

// TestTheSettingsWriteSurfaceReachesNothingButRetention is the structural
// half of the argument above: #171's conclusion holds because a settings
// write cannot reach anything the scheduler acts on unattended. If a
// future section is added to this request type, this fails, and whoever
// adds it has to re-answer the gate-tier question rather than inherit an
// answer that was only ever about retention.
func TestTheSettingsWriteSurfaceReachesNothingButRetention(t *testing.T) {
	var req service.UpdateSettingsRequest
	if got := reflectFieldNames(req); len(got) != 1 || got[0] != "Retention" {
		t.Errorf("UpdateSettingsRequest declares %v; issue #171's argument that this route cannot cause an unattended deletion was made about a retention-only write surface", got)
	}
}

// reflectFieldNames lists a struct's exported field names.
func reflectFieldNames(v any) []string {
	rt := reflect.TypeOf(v)
	out := make([]string, 0, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		if f := rt.Field(i); f.IsExported() {
			out = append(out, f.Name)
		}
	}
	return out
}
