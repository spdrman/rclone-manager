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
//
// KeyFile, KeyEnv and KeyCommand are the three ways (#74) an sftp Source may
// name where its SSH private key comes from. None of them carry key
// material itself, only where to find it: a file path rclone opens itself,
// an environment variable name, or an argv array to run. Exactly one of the
// three may be set for an sftp Source; internal/transport/rclone enforces
// that and resolves whichever is set (see ssh.go's sftpConfig).
type Source struct {
	ID   string
	Type string // "sftp", "local"
	Host string
	Port int
	User string

	// KeyFile is the default and documented preference: the only one of
	// the three that never puts key material in this process's own memory,
	// because rclone opens the file itself.
	KeyFile string

	// KeyEnv names an environment variable to read the key from.
	KeyEnv string

	// KeyCommand is an argv array (KeyCommand[0] is the executable, the
	// rest are its literal arguments) run to produce the key on stdout.
	// It is never a shell string: nothing in this program ever hands it to
	// a shell.
	KeyCommand []string

	// PassphraseFile, PassphraseEnv and PassphraseCommand are the three
	// ways (#269) an sftp Source may name where the passphrase that
	// decrypts its key comes from, mirroring KeyFile/KeyEnv/KeyCommand's
	// own three. At most one may be set; none of them set at all means
	// this key is not passphrase-protected, which is every Source built
	// before #269 existed.
	PassphraseFile    string
	PassphraseEnv     string
	PassphraseCommand []string

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
