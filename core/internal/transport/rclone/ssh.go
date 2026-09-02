// Package rclone: this file owns everything about how the embedded sftp
// backend authenticates and verifies the servers it talks to (FR-6).
//
// I put it in its own file, separate from adapter.go, because the SSH
// posture is a security control, not plumbing. It needs one owner and one
// test file, so a change to it gets reviewed as a security change rather
// than getting buried in a diff to the transport adapter.
//
// The core fact I built this around is rclone's own default. I read
// backend/sftp/sftp.go in the vendored rclone v1.75.0 tree, and the default
// case, reached whenever known_hosts_file, pin_host_key, host_keys and the
// ssh option are all unset, is:
//
//	sshConfig.HostKeyCallback = ssh.InsecureIgnoreHostKey()
//
// That accepts any host key from any server, silently (it logs a notice, but
// does not fail, and does not refuse the connection). If this adapter ever
// forwarded an operator's configuration straight through to rclone, an empty
// or missing known_hosts setting would produce exactly that. sftpConfig
// below is the single place standing between operator configuration and
// rclone's option map, and I built it so that default can never be reached.
package rclone

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/obscure"
	"github.com/rclone/rclone/lib/env"

	"github.com/spdrman/rclone-manager/core/internal/obs"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// sftpConfig turns a transport.Source into the exact rclone sftp backend
// options this adapter is willing to use.
//
// It is deliberately not a pass-through. Every sftp option this function
// does not set is an option this adapter refuses to expose, and that list
// matters as much as the list of what it does set:
//
//   - pass, ask_password, key_use_agent are never set, so password
//     authentication and ssh-agent authentication have no path into this
//     adapter. transport.Source has no password field to begin with, so
//     there is nothing upstream that could even be threaded through, but
//     this function is the backstop: even a future caller that adds a
//     Password field to Source cannot reach a password login without also
//     touching this switch statement.
//   - key_pem (#74) IS reachable, but only just: it is set only when Source
//     names an env or command key resolver instead of key_file, and only
//     ever with what keysource.go's resolveKeyFromEnv/resolveKeyFromCommand
//     returned after confirming it parses (with the resolved passphrase,
//     see below, when one is configured) as an SSH private key. There is
//     no config field anywhere in this repository an operator can put a
//     value into key_pem with directly: config.Remote's Key type has File,
//     Env and Command fields, and deliberately nothing an operator could
//     paste raw key material into (see config.Key's doc). key_file remains
//     the default and documented preference specifically because it is the
//     only one of the three sources that never routes through key_pem at
//     all: rclone opens that file itself, so this adapter's own memory
//     never holds the key.
//   - key_file_pass (#269) IS reachable, the same way key_pem is: only with
//     what passphrase.go's resolvePassphrase returned, never with anything
//     an operator could write into this adapter's option map directly.
//     Unlike key_pem, it is set regardless of which key source is in play
//     (rclone honours it for key_file just as much as for key_pem), and
//     only when Source actually names a passphrase source; a Source with
//     none of PassphraseFile/PassphraseEnv/PassphraseCommand set produces
//     no key_file_pass at all, exactly the case for every Source built
//     before #269 existed. rclone's own key_file_pass option expects an
//     "obscured" (its own reversible obfuscation, not real encryption)
//     value, never the plaintext passphrase, so this file always passes
//     the resolved passphrase through obscure.Obscure before setting it.
//   - pin_host_key and host_keys (rclone's trust-on-first-use pinning mode)
//     are never set. TOFU is a legitimate mode for interactive use, but it
//     means "accept whatever key the server shows the first time", which is
//     the wrong default for an unattended backup job.
//   - ssh (rclone's "shell out to the external ssh binary instead" option)
//     is never set, because that would hand host-key verification to
//     whatever the external ssh binary and its own config happen to do,
//     entirely outside this function's control.
//
// known_hosts is mandatory and is checked against rclone's own escape hatch:
// rclone treats the literal value "none" as an explicit request to disable
// host-key checking (it still calls ssh.InsecureIgnoreHostKey(), it just
// stops logging about it), so a known_hosts value of "none" is refused here
// exactly like an empty one.
func sftpConfig(src transport.Source) (configmap.Simple, error) {
	if src.Host == "" {
		return nil, fmt.Errorf("source %q: host is required for sftp", src.ID)
	}
	if src.User == "" {
		return nil, fmt.Errorf("source %q: user is required for sftp", src.ID)
	}

	// FR-6 + #74: SSH key authentication by default, mandatory rather than
	// optional, exactly as before. rclone's sftp backend, given no
	// key_file, no key_pem, no pass and no ask_password, does not refuse to
	// connect: it falls back to asking a running ssh-agent for a key. That
	// is a real, working authentication path, just not the one this adapter
	// is meant to offer, and an operator who forgot to configure a key would
	// otherwise authenticate against whatever key their agent happens to
	// hold, silently and non-reproducibly. Requiring exactly one of the
	// three sources below closes that path by construction, the same as
	// requiring key_file alone used to, before #74 gave it siblings.
	//
	// "Exactly one", not "at least one": two configured sources is a config
	// mistake, not a precedence order for this adapter to silently pick
	// through, so both are refused rather than one being guessed as
	// intended. internal/config/validate.go enforces this same rule
	// independently, so a config built through that package never reaches
	// here with more than one set; this is the backstop for anything that
	// builds a transport.Source directly, tests included.
	sourceCount := 0
	if src.KeyFile != "" {
		sourceCount++
	}
	if src.KeyEnv != "" {
		sourceCount++
	}
	if len(src.KeyCommand) > 0 {
		sourceCount++
	}
	switch {
	case sourceCount == 0:
		return nil, fmt.Errorf("source %q: exactly one of key_file, key_env or key_command is required for sftp (key-based authentication is mandatory, ssh-agent fallback and password login are not offered)", src.ID)
	case sourceCount > 1:
		return nil, fmt.Errorf("source %q: exactly one of key_file, key_env or key_command may be set for sftp, not more than one", src.ID)
	}

	// #269: the passphrase's own three sources get the identical "exactly
	// one, never more" backstop the key's three sources just got, for the
	// identical reason -- internal/config/validate.go enforces this same
	// rule independently for a config built through that package, so this
	// is the backstop for anything that builds a transport.Source
	// directly, tests included. Zero is fine here, unlike for the key
	// itself: most keys are not passphrase-protected at all, and that is
	// still the default this function assumes when none of the three is
	// set.
	passphraseSourceCount := 0
	if src.PassphraseFile != "" {
		passphraseSourceCount++
	}
	if src.PassphraseEnv != "" {
		passphraseSourceCount++
	}
	if len(src.PassphraseCommand) > 0 {
		passphraseSourceCount++
	}
	if passphraseSourceCount > 1 {
		return nil, fmt.Errorf("source %q: exactly one of key_passphrase_file, key_passphrase_env or key_passphrase_command may be set for sftp, not more than one", src.ID)
	}

	// Resolved before the key material itself, and before known_hosts is
	// even checked: src.KeyEnv/src.KeyCommand's own resolvers (below) need
	// the passphrase to validate the key they resolve actually decrypts
	// with it, and a bad passphrase resolver should be reported as a
	// passphrase problem, not buried behind an unrelated known_hosts
	// error.
	passphraseSecret, hasPassphrase, err := resolvePassphrase(src)
	if err != nil {
		return nil, fmt.Errorf("source %q: resolving the SSH key passphrase: %w", src.ID, err)
	}
	passphrase := ""
	if hasPassphrase {
		passphrase = passphraseSecret.Reveal()
	}

	// keyFileValue and keyPEM are mutually exclusive: exactly one of them
	// ends up populated by the switch below, and which one decides whether
	// the cfg built further down sets key_file or key_pem. Resolution
	// happens here, before known_hosts is even checked, so that a bad
	// resolver (a secrets manager returning junk, an encrypted key, ...)
	// is reported as a key problem, not buried after an unrelated
	// known_hosts error.
	var keyFileValue string
	var keyPEM obs.Secret
	usingKeyPEM := false

	switch {
	case src.KeyFile != "":
		// The default and documented preference (docs/ssh-setup.md): this
		// adapter only ever confirms the file exists, is readable, and
		// still carries the mode it was written with. It never opens it,
		// so the key itself never enters this process's memory at all;
		// rclone's own sftp backend reads key_file directly. A passphrase,
		// if one resolved above, is NOT verified against this file's
		// content here, for the same reason: verifying it would mean
		// reading and decrypting the file in this process, exactly what
		// key_file exists to avoid. rclone itself checks it, with the
		// key_file_pass option set below, the moment this cfg is actually
		// used to connect.
		keyFilePath := env.ShellExpand(src.KeyFile)
		info, err := os.Stat(keyFilePath)
		if err != nil {
			return nil, fmt.Errorf("source %q: key_file %q is not accessible: %w", src.ID, src.KeyFile, err)
		}
		if err := checkKeyFileMode(src.ID, src.KeyFile, info); err != nil {
			return nil, err
		}
		if err := checkKeyDirChainMode(src.ID, src.KeyFile, keyFilePath); err != nil {
			return nil, err
		}
		keyFileValue = src.KeyFile
	case src.KeyEnv != "":
		secret, err := resolveKeyFromEnv(src.KeyEnv, passphrase)
		if err != nil {
			return nil, fmt.Errorf("source %q: resolving the SSH key from environment variable %q: %w", src.ID, src.KeyEnv, err)
		}
		keyPEM = secret
		usingKeyPEM = true
	case len(src.KeyCommand) > 0:
		secret, err := resolveKeyFromCommand(src.KeyCommand, passphrase)
		if err != nil {
			return nil, fmt.Errorf("source %q: resolving the SSH key from the configured command: %w", src.ID, err)
		}
		keyPEM = secret
		usingKeyPEM = true
	}

	// FR-6: host-key verification is mandatory, with no opt-out reachable
	// through this adapter. See the package comment above for exactly what
	// rclone does by default when this is left unset.
	if src.KnownHosts == "" {
		return nil, fmt.Errorf("source %q: known_hosts is required for sftp", src.ID)
	}
	if strings.EqualFold(strings.TrimSpace(src.KnownHosts), "none") {
		return nil, fmt.Errorf("source %q: known_hosts value %q disables rclone's host-key verification, which this adapter refuses to allow", src.ID, src.KnownHosts)
	}
	knownHostsPath := env.ShellExpand(src.KnownHosts)
	if info, err := os.Stat(knownHostsPath); err != nil {
		return nil, fmt.Errorf("source %q: known_hosts %q is not accessible: %w", src.ID, src.KnownHosts, err)
	} else if info.IsDir() {
		return nil, fmt.Errorf("source %q: known_hosts %q is a directory, not a file", src.ID, src.KnownHosts)
	}

	cfg := configmap.Simple{}
	cfg.Set("host", src.Host)
	if src.Port != 0 {
		cfg.Set("port", strconv.Itoa(src.Port))
	}
	cfg.Set("user", src.User)
	if usingKeyPEM {
		// rclone's sftp backend reconstructs the original PEM text with
		// strconv.Unquote("\"" + opt.KeyPem + "\"") (backend/sftp/sftp.go),
		// so the value has to be exactly what strconv.Quote would produce
		// for it, minus the surrounding quotes strconv.Quote adds: the
		// inverse operation of what NewFs does to read it back. Handing it
		// a literal multi-line PEM string instead (real newline bytes, no
		// escaping) fails that Unquote call outright, since a Go
		// interpreted string literal cannot contain a raw newline.
		cfg.Set("key_pem", quoteForRclonePem(keyPEM.Reveal()))
	} else {
		cfg.Set("key_file", keyFileValue)
	}
	if hasPassphrase {
		// rclone's sftp backend reveals key_file_pass with its own
		// obscure.Reveal (backend/sftp/sftp.go), which requires the
		// obscured form, not the plaintext passphrase: Obscure here is the
		// exact inverse of the Reveal rclone performs when it actually
		// opens the key, on the same reasoning quoteForRclonePem states for
		// key_pem's own required transformation. This is honoured
		// regardless of whether the key came from key_file or key_pem
		// above; rclone applies key_file_pass to either.
		obscured, err := obscure.Obscure(passphrase)
		if err != nil {
			return nil, fmt.Errorf("source %q: obscuring the resolved passphrase for rclone: %w", src.ID, err)
		}
		cfg.Set("key_file_pass", obscured)
	}
	cfg.Set("known_hosts_file", src.KnownHosts)

	// fsFor calls info.NewFs directly instead of going through rclone's usual
	// fs.NewFs/fs.ConfigMap path, on purpose: fs.ConfigMap layers in a getter
	// that reads the on-disk rclone config file for a stanza matching the
	// remote name, and this adapter's whole premise is that there is no
	// ambient rclone state to leak in (see the fsFor doc comment). The cost
	// of skipping that path is that none of the sftp backend's own
	// registered option defaults apply either: configstruct.Set only ever
	// reads keys that are actually present in the map, so any option this
	// function leaves unset comes out as its Go zero value, not rclone's
	// documented default.
	//
	// For most sftp options that is harmless, because the zero value already
	// is the intended default (booleans that default to false, strings that
	// default to blank). These three are not like that, and I found this by
	// testing the happy path in ssh_test.go, not by reading the docs: with
	// none of them set, every single sftp operation this adapter makes,
	// including a plain List, fails before it can do anything.
	//
	//   - subsystem: RequestSubsystem(f.opt.Subsystem) is called with the
	//     empty string, and the server it's driving refuses the subsystem
	//     request outright ("subsystem not found") because it never named
	//     one. This isn't really a tunable rclone default so much as it is
	//     the standard SSH2 subsystem name for SFTP, which is why the value
	//     below is a literal rather than something looked up.
	//   - chunk_size and concurrency: rclone passes these straight into
	//     github.com/pkg/sftp's MaxPacketUnchecked and
	//     MaxConcurrentRequestsPerFile, both of which reject anything less
	//     than 1 outright. A zero value doesn't degrade performance, it
	//     fails NewFs for every backend operation, not just transfers, since
	//     they configure the single pooled SFTP client every operation
	//     shares. The values here match rclone's own documented defaults
	//     (32KiB chunks, 64 concurrent requests per file) as of rclone
	//     v1.75.0.
	cfg.Set("subsystem", "sftp")
	cfg.Set("chunk_size", "32Ki")
	cfg.Set("concurrency", "64")

	return cfg, nil
}

// wantKeyFileMode is the permission mode importSSHKeyInto
// (core/service/backupsets.go) writes an imported key with
// (os.WriteFile(path, raw, 0o600)). It is a literal here rather than an
// import of that package's constant, because checkKeyFileMode's whole job
// is to notice when a key's mode has drifted away from what was written,
// and that comparison has to stay honest even if the two files' idea of
// "0600" were ever to drift apart from each other, which is exactly the
// class of silent assumption issue #293 is about.
const wantKeyFileMode = 0o600

// checkKeyFileMode refuses a key_file whose mode is not exactly
// wantKeyFileMode (issue #293), rather than silently trusting that the
// 0600 importSSHKeyInto wrote at creation is still true whenever the key
// is next needed.
//
// This runs inside sftpConfig, so it runs on every call to it: every List,
// Stat, CopyToLocal, RemoteHash and DeleteRemote against an sftp source
// (adapter.go's fsFor/fsForHashing), not only at import. Import-time
// correctness says nothing about whether the mode is still what it was set
// to by the time the key is next used, and a real production deployment
// found exactly that gap: the directory chain down to a backup root had
// drifted to world-writable out of band, most likely during unrelated
// troubleshooting on the host. The cost of checking is one os.Stat the
// key_file case already pays for (see the caller), so there is no added
// I/O over what existed before, and the healthy path (mode unchanged)
// takes exactly the same number of syscalls it always did.
//
// This is an exact-match check, not "no wider than 0600": a mode that only
// ever narrows (say, an operator's own well-meaning 0400) is still not
// what was written, and "verify what actually happened, do not assume a
// write stuck" is the whole point, not "assume a write stuck unless it
// got worse."
//
// # Why this refuses instead of re-asserting 0600
//
// core/service/validator.go's materializeValidators is this project's one
// existing precedent for self-healing a drifted permission: it rewrites an
// embedded validator script, mode included, whenever upToDate finds it has
// drifted, on every run cycle. That precedent does not transfer here,
// because what makes it safe is that the content being restored is
// verified byte-for-byte against an embedded copy this process authored;
// self-healing there can never re-trust content an outside actor supplied.
// A private key has no such reference copy: this process wrote it once,
// from bytes an operator pasted through the wizard, and never kept an
// independent copy to verify against. A chmod alone cannot tell "an
// operator's own chmod widened this" apart from "something replaced the
// file altogether", and a permission drift wide enough to let another
// local actor read this key is exactly the situation where silently
// narrowing the mode back and proceeding, without telling anyone, is the
// wrong instinct. It is HostKeyChanged's own posture (internal/state/
// halts.go, §77 invariant 5) read the same way: a security-relevant trust
// break gets surfaced for a human to look at, not smoothed over so nobody
// ever has to. Refusing with a clear, actionable diagnostic is the safe
// default the issue names for exactly this reason.
func checkKeyFileMode(sourceID, configuredPath string, info os.FileInfo) error {
	if mode := info.Mode().Perm(); mode != wantKeyFileMode {
		return transport.NewError(transport.KeyPermissions, "ssh_key_permissions", fmt.Errorf(
			"source %q: key_file %q has permissions %04o, expected exactly %04o (the mode it was imported with): "+
				"an imported key's permissions must not drift after import; correct it (chmod %04o %s) or re-import the key",
			sourceID, configuredPath, mode, wantKeyFileMode, wantKeyFileMode, configuredPath,
		))
	}
	return nil
}

// checkKeyDirChainMode is checkKeyFileMode's other half: it refuses when
// ANY ancestor directory between the key file and the filesystem root is
// writable by anyone but its owner, rather than trusting that a tight
// mode on the key file itself is enough.
//
// It has to exist because checkKeyFileMode alone leaves exactly the gap
// issue #293's own incident report describes: "the key AND the whole
// directory chain down to the backup root" had drifted to world-writable.
// Unix directory-write permission governs entry changes (unlink, rename,
// create) independent of the target file's own mode bits, so a key left
// at a pristine 0600 sitting inside a world-writable directory is still
// fully exposed: any local actor can delete it and drop a replacement in
// its place, and checkKeyFileMode, looking only at the file, would never
// notice. That is the "more dangerous half" PR #311's own review flagged:
// the file check alone gives false confidence while the actual attack
// surface, swapping the key out entirely, stands untouched.
//
// # How far up: every ancestor, to the filesystem root
//
// There is no configuration field this function can bound the walk to
// instead. sftpConfig only ever receives a transport.Source, which has no
// notion of "the backup root" (that concept, config.Capacity.BackupRoot,
// belongs to a different tree entirely: where backup SETS write their
// output, not where an imported key lives), and threading the full
// manager Config down through Adapter into here for one check would be a
// far bigger change than this fix warrants. Stopping short at some
// arbitrary depth instead would just relocate the blind spot rather than
// remove it: a directory widened one level above wherever the walk gave
// up would be exactly as invisible as the key file itself was before this
// check existed. Walking to the root closes that off completely, "up to
// (at least) the backup root" being trivially true of every path. The
// cost is a handful of extra stat calls per connection, negligible next
// to the SSH handshake this check already gates.
//
// # The sticky-bit exception
//
// A directory whose mode has the sticky bit set (os.ModeSticky — 1777,
// the standard permissions of /tmp on every mainstream Unix) is not
// refused merely for being group- or world-writable. This is not a
// loophole; it is the same fact that makes a world-writable /tmp safe to
// have on a real system in the first place: POSIX restricts unlink and
// rename inside a sticky directory to the entry's own owner, the
// directory's owner, or root, regardless of who else can write there,
// which is exactly the attack this function exists to close. A check that
// ignored the sticky bit would refuse an entirely ordinary, correctly
// configured deployment the moment a key file's path happened to share an
// ancestor with the system's shared temp directory, which is a false
// positive this project has no reason to accept for a bit POSIX itself
// already uses to neutralize the risk.
func checkKeyDirChainMode(sourceID, configuredPath, keyFilePath string) error {
	dir, err := filepath.Abs(filepath.Dir(keyFilePath))
	if err != nil {
		return fmt.Errorf("source %q: resolving the directory containing key_file %q: %w", sourceID, configuredPath, err)
	}
	for {
		info, err := os.Stat(dir)
		if err != nil {
			return fmt.Errorf("source %q: checking permissions on %q, a directory containing key_file %q: %w", sourceID, dir, configuredPath, err)
		}
		mode := info.Mode()
		if mode.Perm()&0o022 != 0 && mode&os.ModeSticky == 0 {
			return transport.NewError(transport.KeyPermissions, "ssh_key_permissions", fmt.Errorf(
				"source %q: key_file %q has a containing directory %q with permissions %04o: a group- or world-writable "+
					"directory lets any local actor delete or replace the key regardless of the key file's own mode; "+
					"correct it (chmod go-w %s) or move the key",
				sourceID, configuredPath, dir, mode.Perm(), dir,
			))
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
		dir = parent
	}
}

// quoteForRclonePem converts raw PEM text into the single-line,
// backslash-n-escaped form rclone's sftp backend expects for its key_pem
// option. backend/sftp/sftp.go reconstructs the original bytes with
// strconv.Unquote("\"" + opt.KeyPem + "\""), so what this function returns
// has to be exactly the inverse: strconv.Quote(pem) with the surrounding
// quotes that Quote adds trimmed back off, since those two already are
// exact inverses of each other for every byte a valid PEM block can
// contain, not just the newlines rclone's own help text happens to call
// out.
func quoteForRclonePem(pem string) string {
	quoted := strconv.Quote(pem)
	return quoted[1 : len(quoted)-1]
}

// withSHA256 returns cfg with the two options rclone needs before it will
// compute a SHA-256 digest on an sftp source, and is applied ONLY on the
// path that asks for one (Adapter.RemoteHash). It is deliberately not part
// of sftpConfig, so the Fs that lists and copies is built exactly as it was
// before this existed.
//
// # Why the hash has to be asked for at all
//
// rclone's sftp backend builds its candidate hash set from its own "hashes"
// option, and when that option is empty (backend/sftp/sftp.go, Hashes()) it
// seeds the set with hash.MD5 and hash.SHA1 and nothing else. SHA-256 is
// never a candidate, so the probe that would look for a working sha256sum
// never runs, Hashes() comes back without it on every server however
// capable, and Adapter.RemoteHash's capability check refuses before it ever
// reaches the object. config.Validation.Hash accepts "" or "sha256" and
// nothing else, so leaving this unset made the only non-empty value it
// accepts unusable against the only remote backend this project ships
// besides "local": every artifact in such a backup set went
// TRANSFERRED -> VERIFYING -> FAILED, forever, on every host.
//
// # Why naming it is not enough
//
// rclone will not trust a hash command until it has probed it, and its
// v1.75.0 SHA-256 probe list pairs the sha256 commands with the SHA-1 ones
// for the empty-input check:
//
//	{"sha256sum", "sha1sum"}, {"sha256 -r", "sha1 -r"},
//	{"rclone hashsum sha256", "rclone hashsum sha256"}
//
// checkHash runs the second element and compares its output against
// SHA-256's digest of empty input. It runs sha1sum, gets SHA-1's, and
// rejects a working sha256sum. Only the third candidate can ever be
// accepted, and that one needs rclone installed on the SOURCE host, which
// nothing entitles a backup source to have. Measured against a real sshd
// with coreutils sha256sum on PATH: with "hashes" alone, RemoteHash still
// answered `backend "sftp" cannot compute sha256`. checkHash skips its
// whole probe when the command is already set, so pinning it is rclone's
// own supported way past a table this project does not own.
//
// # Why this is not folded into sftpConfig
//
// Because it would stop backups working. rclone's copy picks an integrity
// hash from Common(src.Hashes(), dst.Hashes()). With these options on the
// Fs that copies, a hardened, shell-less account (the posture
// docs/ssh-setup.md recommends) advertises SHA-256, fails to compute it at
// copy time, and rclone compares the empty string against the local digest
// and reports `corrupted on transfer: sha256 hashes differ`. Measured: it
// turns that deployment from "backs up, cannot hash-verify" into "cannot
// back up at all", and it does so with a message that blames corruption
// for a missing capability. It breaks the no-hash case too, which never
// asked for any of this.
//
// Confining it to RemoteHash keeps the copy path byte-identical and leaves
// exactly one behaviour changed: an account that CAN run sha256sum is now
// allowed to. One that cannot still fails, because rclone runs the command
// and the run fails, so Verify fails the artifact exactly as it did when
// the capability was reported absent. The message changes from "backend
// \"sftp\" cannot compute sha256" to one naming the command that could not
// run, which is more use to whoever has to fix it.
func withSHA256(cfg configmap.Simple) configmap.Simple {
	out := configmap.Simple{}
	for k, v := range cfg {
		out[k] = v
	}
	out.Set("hashes", "sha256")
	out.Set("sha256sum_command", "sha256sum")
	return out
}
