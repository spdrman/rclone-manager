package webhost

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/service"
)

func int64p(v int64) *int64 { return &v }

// seedOperation puts op into the fake backend so a read route has
// something to serve. The fake's own submit path cannot produce a live
// progress reading, because progress comes from a running cycle and this
// fake never runs one.
func seedOperation(f *syncFakeBackend, op service.Operation) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops[op.ID] = op
}

func getOperation(t *testing.T, router http.Handler, id string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/operations/"+id, nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/operations/%s = %d, want 200; body: %s", id, rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return body
}

// TestGetOperation_SerializesLiveProgress is the wire half of issue #221:
// what core/service measured is what a polling client actually receives,
// under the names api/v1/openapi.json declares.
func TestGetOperation_SerializesLiveProgress(t *testing.T) {
	tr := newOperationsTestRouter(t, alwaysPassGate{})
	observed := time.Date(2026, 8, 30, 9, 15, 0, 0, time.UTC)
	seedOperation(tr.backend, service.Operation{
		ID:     "op_live",
		Action: service.ActionRunCycle,
		Status: "running",
		Progress: &service.OperationProgress{
			ObservedAt:       observed,
			Sequence:         12,
			Stage:            "transferring",
			BackupSetID:      "alpha/nightly",
			BackupSetsDone:   1,
			BackupSetsTotal:  3,
			Artifact:         "nightly.dump",
			ArtifactsDone:    4,
			BytesTransferred: int64p(512),
			BytesTotal:       int64p(2048),
			BytesPerSecond:   int64p(128),
		},
	})

	body := getOperation(t, tr.router, "op_live")
	raw, ok := body["progress"]
	if !ok {
		t.Fatalf("the response carries no progress object at all: %v", body)
	}
	progress, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("progress = %#v, want an object", raw)
	}

	for field, want := range map[string]any{
		"observed_at":       observed.Format(time.RFC3339Nano),
		"sequence":          float64(12),
		"stage":             "transferring",
		"backup_set_id":     "alpha/nightly",
		"backup_sets_done":  float64(1),
		"backup_sets_total": float64(3),
		"artifact":          "nightly.dump",
		"artifacts_done":    float64(4),
		"bytes_transferred": float64(512),
		"bytes_total":       float64(2048),
		"bytes_per_second":  float64(128),
	} {
		if got := progress[field]; got != want {
			t.Errorf("progress.%s = %#v, want %#v", field, got, want)
		}
	}
}

// TestGetOperation_OmitsProgressEntirelyWhenThereIsNone is the assertion
// the whole feature turns on. An operation with no live reading must send
// NO progress key: not a null, and above all not an object full of zeroes,
// which a client would render as a transfer that exists and has moved
// nothing.
func TestGetOperation_OmitsProgressEntirelyWhenThereIsNone(t *testing.T) {
	tr := newOperationsTestRouter(t, alwaysPassGate{})
	for _, status := range []string{"queued", "running", "completed", "failed"} {
		seedOperation(tr.backend, service.Operation{
			ID: "op_" + status, Action: service.ActionRunCycle, Status: status,
		})
	}

	for _, status := range []string{"queued", "running", "completed", "failed"} {
		body := getOperation(t, tr.router, "op_"+status)
		if raw, present := body["progress"]; present {
			t.Errorf("a %s operation with no reading serialized progress = %#v; the key must be absent, because absent is the only encoding a client can tell apart from zero",
				status, raw)
		}
	}
}

// TestGetOperation_SerializesAMeasuredZero is the twin of the test above,
// and the reason the byte fields are pointers rather than plain int64s
// with omitempty: a copy that has started and moved nothing has a real
// reading of zero, and it has to survive encoding.
func TestGetOperation_SerializesAMeasuredZero(t *testing.T) {
	tr := newOperationsTestRouter(t, alwaysPassGate{})
	seedOperation(tr.backend, service.Operation{
		ID: "op_zero", Action: service.ActionRunCycle, Status: "running",
		Progress: &service.OperationProgress{
			ObservedAt:       time.Date(2026, 8, 30, 9, 15, 0, 0, time.UTC),
			Sequence:         1,
			Stage:            "transferring",
			Artifact:         "nightly.dump",
			BytesTransferred: int64p(0),
			BytesTotal:       int64p(2048),
		},
	})

	progress, ok := getOperation(t, tr.router, "op_zero")["progress"].(map[string]any)
	if !ok {
		t.Fatal("a measured zero was dropped from the response entirely")
	}
	got, present := progress["bytes_transferred"]
	if !present {
		t.Fatal("bytes_transferred is absent for a measured zero; absent means unmeasured and this was measured")
	}
	if got != float64(0) {
		t.Errorf("bytes_transferred = %#v, want 0", got)
	}
	if _, present := progress["bytes_per_second"]; present {
		t.Error("bytes_per_second is present when no rate was measured; an unmeasured rate must be absent, not zero")
	}
}

// TestOperationProgress_StagesAreExactlyTheContractsEnum stops the two
// vocabularies drifting. core/service names the stages a cycle reports and
// this package serves those strings verbatim, so a stage renamed on one
// side and not the other would reach a client as a value the contract does
// not declare, and no other check in this repository would notice.
func TestOperationProgress_StagesAreExactlyTheContractsEnum(t *testing.T) {
	path := filepath.Join("..", "..", "..", "api", "v1", "openapi.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the contract at %s: %v", path, err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]struct {
					Enum []string `json:"enum"`
				} `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing the contract: %v", err)
	}
	declared := doc.Components.Schemas["OperationProgress"].Properties["stage"].Enum
	if len(declared) == 0 {
		t.Fatal("the contract declares no OperationProgress.stage enum, so this comparison would pass vacuously")
	}
	if len(service.OperationStages) == 0 {
		t.Fatal("core/service reports no stages at all, so this comparison would pass vacuously")
	}
	if len(declared) != len(service.OperationStages) {
		t.Fatalf("the contract declares %v and core/service reports %v", declared, service.OperationStages)
	}
	for i := range declared {
		if declared[i] != service.OperationStages[i] {
			t.Errorf("stage %d: the contract says %q, core/service says %q", i, declared[i], service.OperationStages[i])
		}
	}
}
