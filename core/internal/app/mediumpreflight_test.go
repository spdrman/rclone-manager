package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/obs"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// preflightStore is a MediumStore that behaves like a working bucket, so
// the tests here are about what this package DOES with a preflight rather
// than about the preflight itself (internal/mediumcheck owns that).
type preflightStore struct {
	objects map[string][]byte
	statErr error
}

func newPreflightStore() *preflightStore { return &preflightStore{objects: map[string][]byte{}} }

func (p *preflightStore) StatObject(_ context.Context, _ transport.Medium, key string) (transport.ObjectInfo, error) {
	if p.statErr != nil {
		return transport.ObjectInfo{}, p.statErr
	}
	body, ok := p.objects[key]
	if !ok {
		return transport.ObjectInfo{}, transport.NewError(transport.NotFound, "stat_object", errors.New("no such key"))
	}
	return transport.ObjectInfo{Key: key, Size: int64(len(body)), StorageClass: config.StorageClassStandard}, nil
}

func (p *preflightStore) UploadFromLocal(_ context.Context, _ transport.Medium, localPath, key string, _ transport.UploadOptions) (transport.UploadResult, error) {
	body, err := osReadFileForTest(localPath)
	if err != nil {
		return transport.UploadResult{}, err
	}
	p.objects[key] = body
	return transport.UploadResult{Key: key, BytesUploaded: int64(len(body))}, nil
}

func (p *preflightStore) OpenObject(_ context.Context, _ transport.Medium, key string) (io.ReadCloser, error) {
	body, ok := p.objects[key]
	if !ok {
		return nil, transport.NewError(transport.NotFound, "open_object", errors.New("no such key"))
	}
	return io.NopCloser(strings.NewReader(string(body))), nil
}

func (p *preflightStore) ObjectChecksum(context.Context, transport.Medium, string, transport.HashAlgorithm) (transport.ChecksumAttestation, error) {
	return transport.ChecksumAttestation{}, transport.NewError(transport.UnsupportedCapability, "object_checksum", errors.New("no full-object digest"))
}

func (p *preflightStore) DeleteObject(_ context.Context, _ transport.Medium, key string) error {
	delete(p.objects, key)
	return nil
}

func (p *preflightStore) ListObjects(context.Context, transport.Medium, string) ([]transport.ObjectInfo, error) {
	return nil, errors.New("not used")
}

func (p *preflightStore) RestoreStatus(context.Context, transport.Medium, string) (*transport.RestoreState, error) {
	return nil, errors.New("not used")
}

func (p *preflightStore) InitiateRestore(context.Context, transport.Medium, string, int) error {
	return errors.New("not used")
}

var _ transport.MediumStore = (*preflightStore)(nil)

// preflightCanary is a value that exists nowhere else in this repository,
// so finding it in an output proves where it came from. The E1.3 shape,
// aimed at this surface.
const preflightCanary = "CANARY-443-app-6d0f28ba91c7-DO-NOT-SERVE"

func preflightConfig(mediums ...config.StorageMedium) *config.Config {
	return &config.Config{StorageMediums: mediums}
}

func workingMedium() config.StorageMedium {
	return config.StorageMedium{
		ID:          "offsite_s3",
		Type:        config.StorageMediumTypeS3,
		Region:      "us-east-1",
		Bucket:      "nas-backups",
		Credentials: config.MediumCredentials{Env: "BACKUP_S3_" + preflightCanary},
	}
}

func TestPreflightMedium_RunsAgainstADeclaredMedium(t *testing.T) {
	svc := New(preflightConfig(workingMedium()), nil, nil, nil)
	svc.MediumStore = newPreflightStore()

	report, err := svc.PreflightMedium(context.Background(), "offsite_s3")
	if err != nil {
		t.Fatalf("PreflightMedium: %v", err)
	}
	if !report.OK {
		t.Fatalf("a working medium did not pass: %+v", report.Failures())
	}
	if report.Medium != "offsite_s3" {
		t.Fatalf("report is for %q, want offsite_s3", report.Medium)
	}
}

func TestPreflightMedium_AnUndeclaredMediumIsANamedRefusal(t *testing.T) {
	svc := New(preflightConfig(workingMedium()), nil, nil, nil)
	svc.MediumStore = newPreflightStore()

	_, err := svc.PreflightMedium(context.Background(), "typo_s3")
	if err == nil {
		t.Fatal("preflighting a medium the configuration does not declare succeeded")
	}
	if !AsMediumNotDeclared(err) {
		t.Fatalf("err = %v, want it to carry ErrMediumNotDeclared so the layers above give it a name rather than a 500", err)
	}

	// The reserved local id is not a medium, and answering it with a
	// preflight would be answering about a place that has no bucket and no
	// endpoint at all.
	if _, err := svc.PreflightMedium(context.Background(), config.MediumLocal); !AsMediumNotDeclared(err) {
		t.Fatalf("preflighting %q gave %v, want the same named refusal", config.MediumLocal, err)
	}
}

func TestPreflightMedium_RefusesWithoutAWayToReachOne(t *testing.T) {
	svc := New(preflightConfig(workingMedium()), nil, nil, nil)
	if _, err := svc.PreflightMedium(context.Background(), "offsite_s3"); err == nil {
		t.Fatal("preflight ran with no MediumStore at all")
	}
}

// TestPreflightMedium_TheClassifiedCauseGoesToTheLogAndNotTheReport is
// FR-33 at this layer, with the positive control that makes it mean
// something: the canary has to reach the log, or the test is passing
// against a preflight that reported nothing.
func TestPreflightMedium_TheClassifiedCauseGoesToTheLogAndNotTheReport(t *testing.T) {
	var logged strings.Builder
	svc := New(preflightConfig(workingMedium()), nil, nil, obs.New(&logged, obs.LevelInfo))

	store := newPreflightStore()
	store.statErr = transport.NewError(transport.Configuration, "medium_credentials",
		errors.New("medium \"offsite_s3\": resolving credentials from environment variable \"BACKUP_S3_"+preflightCanary+"\": not set"))
	svc.MediumStore = store

	report, err := svc.PreflightMedium(context.Background(), "offsite_s3")
	if err != nil {
		t.Fatalf("PreflightMedium: %v", err)
	}
	if report.OK {
		t.Fatal("a medium whose credential could not be resolved reported OK")
	}

	rendered, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(rendered), preflightCanary) {
		t.Fatalf("the preflight report carries the canary:\n%s", rendered)
	}
	if !strings.Contains(logged.String(), preflightCanary) {
		t.Fatalf("the canary never reached the log either, so this test proved nothing about redaction:\n%s", logged.String())
	}
}

// TestPreflightMedium_IsNeverReachedFromACycle is a structural check
// rather than a behavioural one, and it is here because the cost of being
// wrong is somebody's bucket getting a probe object written to it every
// poll interval forever. It reads this package's own source rather than
// running anything, which is the only way to assert an absence.
func TestPreflightMedium_IsNeverReachedFromACycle(t *testing.T) {
	for _, file := range []string{"cycle.go", "daemon.go", "pipeline.go", "movecycle.go", "reconcile.go"} {
		body, err := osReadFileForTest(file)
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		for _, forbidden := range []string{"PreflightMedium", "mediumcheck."} {
			if strings.Contains(string(body), forbidden) {
				t.Errorf("%s reaches for %q; a preflight writes a probe object to an operator's bucket and must only ever run because a person asked", file, forbidden)
			}
		}
	}
}

// osReadFileForTest keeps the one os.ReadFile this file needs in one
// place, named so it cannot be confused with anything the package itself
// does with the filesystem.
func osReadFileForTest(path string) ([]byte, error) { return os.ReadFile(path) }
