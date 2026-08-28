// Package transport is the manager-owned boundary around whatever moves bytes.
//
// Every rclone import in this repository lives under transport/rclone. Nothing
// else may import rclone, because the whole point of embedding rather than
// forking is that upstream API churn stays contained in one adapter (FR-3).
//
// Note what is absent: there is no Move. Copy, verify, commit and delete are
// four separately owned steps, and a Move collapses them into one and takes the
// delete decision away from the lifecycle manager (FR-11).
package transport

import "context"

// HashAlgorithm names a checksum the manager may ask a backend for.
type HashAlgorithm string

const SHA256 HashAlgorithm = "sha256"

// Source identifies one configured remote.
type Source struct {
	ID         string
	Type       string // "sftp", "local"
	Host       string
	Port       int
	User       string
	KeyFile    string
	KnownHosts string
	Root       string
}

// RemoteArtifact is the identity of a remote object at a point in time.
//
// The manager persists this at discovery and compares it again immediately
// before deleting, so a remote file that was replaced under a reused pathname
// is refused rather than destroyed (FR-16).
type RemoteArtifact struct {
	Path    string
	Size    int64
	ModTime int64 // unix seconds; 0 when the backend does not report one
	Hash    string
	HashAlg HashAlgorithm
	ID      string // backend-specific stable identifier, empty when unavailable
}

// TransferResult reports what a copy actually did.
type TransferResult struct {
	BytesTransferred int64
	Checksummed      bool
}

// Transport is the only surface lifecycle code is allowed to depend on.
type Transport interface {
	List(ctx context.Context, source Source) ([]RemoteArtifact, error)
	Stat(ctx context.Context, source Source, remotePath string) (RemoteArtifact, error)
	CopyToLocal(ctx context.Context, source Source, remotePath, localPartialPath string) (TransferResult, error)
	RemoteHash(ctx context.Context, source Source, remotePath string, algorithm HashAlgorithm) (string, error)
	DeleteRemote(ctx context.Context, source Source, remotePath string) error
}
