package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/apicontract"
	"github.com/spdrman/rclone-manager/core/service"
)

// The engine the read commands in issue #544 are asked to agree with, and
// why it is built the way it is.
//
// core may not import apps, so there is no way from this package to stand
// up the real /api/v1: its router, its authentication and its handlers all
// live in apps/common. What CAN be reached is the layer underneath them,
// core/service, which is the same *BackupService the real handlers are
// written over. So this serves the read half of the contract out of a
// BackupService of its own, opened independently of the one the command
// under test is using.
//
// Independently is the whole point. Two BackupServices over one deployment
// is exactly the shape #535 recorded: the engine read its configuration
// when it started and the CLI reads whatever is on disk now. Backing this
// with the same service object the command uses would make agreement true
// by construction and prove nothing, so it is not shared, and
// startDivergedReadEngine below hands this one a DIFFERENT configuration
// file so the disagreement can be arranged deliberately.
//
// It also takes the serving lock, because a read command decides its mode
// from that lock and not from a port. AnnounceServing in this process
// works for the reason core/service.RunningEngine's own doc gives: flock
// attaches to the open file description, so a process that announces and
// then asks finds itself. That is what a real engine looks like to a probe
// -- announce first, then serve -- and here one object does both, which is
// closer to the thing being modelled than a subprocess that announces
// while an unrelated goroutine serves.

// readEngine is a stand-in engine: the serving announcement a CLI probes
// for, plus the read half of /api/v1 served from core/service.
type readEngine struct {
	svc     *service.BackupService
	server  *httptest.Server
	baseURL string

	// stale makes every answer below drop one item, or flip one verdict,
	// while GET /system/version goes on reporting the real configuration
	// revision.
	//
	// That combination is the one the revision comparison cannot see, and
	// it is the shape a caching bug or a half-applied hot reload actually
	// takes: the two processes agree about which configuration is in force
	// and disagree about what it means. Without it, the per-command
	// questions in readagreement.go would never have been watched fail,
	// and a guard nobody has watched fail is not a guard.
	stale bool

	mu   sync.Mutex
	seen []string
}

// startReadEngine serves enginePath's configuration at an HTTP address,
// and announces itself as serving the deployment configPath names.
//
// The two paths are separate arguments so that a test can point the engine
// at a configuration that is NOT the one the command under test loads,
// which is the only way to arrange the divergence this issue exists to
// catch. They name the same journal, because the journal is what a
// deployment is.
func startReadEngine(t *testing.T, configPath, enginePath string) *readEngine {
	t.Helper()

	release, err := service.AnnounceServing(configPath)
	if err != nil {
		t.Fatalf("announcing this process as serving %s: %v", configPath, err)
	}
	t.Cleanup(func() { _ = release() })

	svc, closeSvc, err := service.Open(context.Background(), enginePath)
	if err != nil {
		t.Fatalf("opening the engine's own service on %s: %v", enginePath, err)
	}
	t.Cleanup(func() { _ = closeSvc() })

	e := &readEngine{svc: svc}
	e.server = httptest.NewServer(http.HandlerFunc(e.serve))
	t.Cleanup(e.server.Close)
	e.baseURL = e.server.URL
	return e
}

// use points this invocation's CLI at this engine, the way an operator
// would before running a command from a host.
func (e *readEngine) use(t *testing.T) {
	t.Helper()
	t.Setenv(engineURLEnv, e.baseURL)
	t.Setenv(engineUsernameEnv, "operator")
	t.Setenv(enginePasswordEnv, "correct-horse-battery")
}

// asked returns the contract paths this engine was actually asked for, in
// order, so a test can tell a command that put its question to the engine
// apart from one that only looked like it did.
func (e *readEngine) asked() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.seen...)
}

// dropLast is how `stale` is applied: one fewer of whatever was asked
// for. One is enough, and it is the smallest difference the comparison has
// to notice.
func dropLast[T any](in []T, stale bool) []T {
	if stale && len(in) > 0 {
		return in[:len(in)-1]
	}
	return in
}

// serve routes by the contract's own paths, so a client that asks for a
// route the contract does not declare gets the 404 a real deployment would
// give it rather than a helpful answer this file invented.
func (e *readEngine) serve(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rel, ok := strings.CutPrefix(r.URL.EscapedPath(), apicontract.BasePath)
	if !ok {
		http.NotFound(w, r)
		return
	}
	e.mu.Lock()
	e.seen = append(e.seen, rel)
	e.mu.Unlock()
	segments := strings.Split(strings.TrimPrefix(rel, "/"), "/")

	switch {
	case rel == "/auth/session":
		// A live session, named, because that is what the client's own
		// contract check requires of this response.
		e.write(w, apicontract.SessionResponse{Username: "operator"})

	case rel == "/system/version":
		e.write(w, apicontract.VersionResponse{
			APIVersion:     apicontract.Version,
			ConfigRevision: e.svc.ConfigRevision(),
			Configured:     true,
			Ready:          e.svc.Ready(),
		})

	case rel == "/backup-sets":
		sets, err := e.svc.ListBackupSets(ctx)
		if err != nil {
			e.fail(w, err)
			return
		}
		out := apicontract.ListBackupSetsResponse{BackupSets: []apicontract.BackupSet{}}
		for _, bs := range dropLast(sets, e.stale) {
			out.BackupSets = append(out.BackupSets, apicontract.BackupSet{
				ID: bs.ID, SourceName: bs.SourceName, Name: bs.Name,
				Host: bs.Host, Port: bs.Port, User: bs.User,
				RemotePath: bs.RemotePath, LocalPath: bs.LocalPath,
				Include:             bs.Include,
				CompletionStrategy:  bs.CompletionStrategy,
				StableForSeconds:    int(bs.StableFor / time.Second),
				ValidatorID:         string(bs.ValidatorID),
				Disabled:            bs.Disabled,
				ReadOnly:            bs.ReadOnly,
				RetentionIsOverride: bs.RetentionIsOverride,
			})
		}
		e.write(w, out)

	case rel == "/backups":
		filter := service.ArtifactFilter{BackupSetID: r.URL.Query().Get("setId")}
		artifacts, err := e.svc.ListArtifacts(ctx, filter)
		if err != nil {
			e.fail(w, err)
			return
		}
		out := apicontract.ListArtifactsResponse{Artifacts: []apicontract.Artifact{}}
		for _, a := range dropLast(artifacts, e.stale) {
			out.Artifacts = append(out.Artifacts, wireArtifact(a))
		}
		e.write(w, out)

	case len(segments) == 4 && segments[0] == "backups":
		a, err := e.svc.GetArtifact(ctx, strings.Join(segments[1:], "/"))
		if err != nil {
			e.fail(w, err)
			return
		}
		wire := wireArtifact(a)
		if e.stale {
			// A state this artifact was in earlier, which is what a
			// process answering from a cached row would say.
			wire.State = "DISCOVERED"
		}
		e.write(w, wire)

	case rel == "/system/health":
		report, err := e.svc.Health(ctx)
		if err != nil {
			e.fail(w, err)
			return
		}
		out := apicontract.HealthResponse{BackupSets: []apicontract.BackupSetHealth{}}
		for _, bs := range dropLast(report.BackupSets, e.stale) {
			out.BackupSets = append(out.BackupSets, apicontract.BackupSetHealth{
				BackupSetID: bs.BackupSetID,
				SourceName:  bs.SourceName,
				SetName:     bs.SetName,
				State:       bs.State,
				Reason:      bs.Reason,
			})
		}
		e.write(w, out)

	case len(segments) == 5 && segments[0] == "backup-sets" && segments[3] == "retention" && segments[4] == "preview":
		plan, err := e.svc.PreviewRetention(ctx, segments[1], segments[2])
		if err != nil {
			e.fail(w, err)
			return
		}
		out := apicontract.RetentionPlan{
			BackupSetID:         plan.BackupSetID,
			ConfigRevision:      plan.ConfigRevision,
			PlanID:              plan.PlanID,
			KeepCount:           plan.KeepCount,
			DeleteCount:         plan.DeleteCount,
			RetentionIsOverride: plan.RetentionIsOverride,
			Verdicts:            []apicontract.RetentionVerdict{},
		}
		for _, v := range plan.Verdicts {
			action := v.Action
			if e.stale {
				action = "DELETE"
				if v.Action == "DELETE" {
					action = "KEEP"
				}
			}
			out.Verdicts = append(out.Verdicts, apicontract.RetentionVerdict{
				Action: action, Artifact: v.Artifact, Reason: v.Reason,
			})
		}
		e.write(w, out)

	default:
		http.NotFound(w, r)
	}
}

func wireArtifact(a service.Artifact) apicontract.Artifact {
	return apicontract.Artifact{
		ID: a.ID, BackupSetID: a.BackupSetID,
		SourceName: a.SourceName, SetName: a.SetName, Name: a.Name,
		RemotePath: a.RemotePath, LocalPath: a.LocalPath,
		State: a.State, RetentionPolicy: a.RetentionPolicy,
	}
}

func (e *readEngine) write(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(body)
}

// fail answers with the envelope apps/common/webhost uses, so the client
// reads a refusal here the way it would read one from the real engine.
func (e *readEngine) fail(w http.ResponseWriter, err error) {
	var body apicontract.ErrorResponse
	body.Error.Code = apicontract.ErrorCodeInternal
	body.Error.Message = err.Error()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusInternalServerError)
	_ = json.NewEncoder(w).Encode(body)
}
