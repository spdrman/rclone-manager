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

	// KeyEncryptionFile, KeyEncryptionEnv and KeyEncryptionCommand are the
	// three ways (#298) this manager may name where the key that protects
	// KeyFile's content at rest comes from, mirroring KeyFile/KeyEnv/
	// KeyCommand's own three in shape. Unlike those three, this is a
	// config-wide setting, not a per-Source one -- every Source built
	// from the same *config.Config carries the same three values here,
	// see internal/app's sourceFor -- but it travels on Source anyway
	// rather than through a separate parameter, because internal/
	// transport/rclone's sftpConfig is the one place that already reads
	// KeyFile and is the only place that can act on this. None of them
	// set at all (every Source built before #298 existed) means KeyFile,
	// if set, is read and used exactly as before: an on-disk key this
	// manager never opens itself.
	KeyEncryptionFile    string
	KeyEncryptionEnv     string
	KeyEncryptionCommand []string

	KnownHosts string

	// MaxConnections caps how many simultaneous SFTP connections this
	// source may open, mapping to rclone's sftp `connections` option. Zero
	// means unset, which is rclone's own default of unlimited and is what
	// every Source built before #264 existed means.
	//
	// This is not the same setting as the per-file request window (rclone's
	// `concurrency`, which internal/transport/rclone pins at 64). That one
	// governs how many requests are outstanding inside one connection and
	// says nothing about how many connections get opened, which is exactly
	// the confusion that let this go unnoticed: a source can look
	// thoroughly tuned for concurrency and still open an unbounded number
	// of connections.
	//
	// It exists because a hardened host can refuse the connection rather
	// than queue it. Both production sources this manager pulls from carry
	// an iptables rule rejecting a third simultaneous SSH connection from
	// one address with a TCP reset, so an unbounded transfer does not run
	// slowly, it fails, and it fails as a bare "connection refused" that
	// names nothing an operator could act on.
	MaxConnections int

	Root string
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
