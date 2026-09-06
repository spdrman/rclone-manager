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

// SHA256 is the only algorithm this boundary speaks, and the absence of a
// second constant is the enforcement rather than an omission. FR-32 holds
// only if there is no way to ask a backend for the weaker checksum it
// would otherwise hand back, and config.Validation accepts "" or "sha256"
// and nothing else, so a second value here would be a capability no
// configuration could reach and a comparison nothing should make.
//
// MediumStore.ObjectChecksum in medium.go names the weaker checksum FR-32
// is about and explains why nothing here carries one. This file cannot
// repeat that explanation and does not try: internal/placement has a guard
// that keeps the word itself out of production code, precisely so there is
// nothing anywhere to compare a content hash against, and it admits only
// the four files that exist to say why they hold none. Writing this
// paragraph the obvious way is what turned that guard red.
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

	// MaxConnections caps how many simultaneous SFTP connections ONE
	// OPERATION against this source may open, mapping to rclone's sftp
	// `connections` option. Zero means unset, which is rclone's own
	// default of unlimited and is what every Source built before #264
	// existed means.
	//
	// Per operation, not per host, and the wording is deliberate (#355).
	// rclone's token dispenser lives on an Fs, and internal/transport/rclone
	// builds one Fs per operation, so two operations against one host are
	// two independent budgets. The daemon and the web API's own
	// reachability check are exactly that case.
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
	// names nothing an operator could act on. What actually holds this
	// manager under such a cap is the adapter's own bound of one connection
	// per operation (oneConnectionAtATime in adapter.go); this is the
	// belt over that, enforced by rclone itself.
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
//
// One field, and the one that is absent is the interesting one. This type
// used to carry a Checksummed bool saying the copy had compared a hash of
// its own, which internal/lifecycle/verify.go read as a verification
// already performed and used to skip its own RemoteHash call. No
// production copy ever set it, so the shortcut was dead, and #492 removed
// both rather than wiring it up, because wiring it up honestly is the
// worse of the two outcomes.
//
// rclone's copy does compare a hash, and it picks which one with
// operations.CommonHash: the first type the two sides share, in the order
// its own hash package registered them. The weaker checksum registers
// first, so on a local destination it is the answer against local and
// against sftp alike, and it is the only answer an s3 medium can give at
// all. This boundary speaks one algorithm (see SHA256 above, and
// MediumStore.ObjectChecksum in medium.go for why the other one is named
// nowhere it could be compared against anything). A field reporting "the
// copy compared SOMETHING" is therefore a field that can only ever mean
// "the copy compared the weaker one", and a `hash: sha256` policy
// discharged by that is the silent downgrade of configured verification
// FR-13 forbids.
//
// state.TransferResult still has the field, because it is a column in
// shipped, immutable migrations. Nothing writes it any more, and
// verify.go no longer reads it.
type TransferResult struct {
	// BytesTransferred is what the destination reports it holds after the
	// copy, read off the written object rather than counted on the way
	// past, so it is a statement about what landed and not about what was
	// sent.
	BytesTransferred int64
}

// Transport is the only surface lifecycle code is allowed to depend on.
type Transport interface {
	List(ctx context.Context, source Source) ([]RemoteArtifact, error)
	Stat(ctx context.Context, source Source, remotePath string) (RemoteArtifact, error)
	CopyToLocal(ctx context.Context, source Source, remotePath, localPartialPath string) (TransferResult, error)
	RemoteHash(ctx context.Context, source Source, remotePath string, algorithm HashAlgorithm) (string, error)
	DeleteRemote(ctx context.Context, source Source, remotePath string) error
}
