// Package rclone: this file owns everything about how the embedded sftp
// backend authenticates and verifies the servers it talks to (FR-6).
//
// It exists as its own file, separate from adapter.go, because the SSH
// posture is a security control, not plumbing. It needs one owner and one
// test file, so a change to it is reviewed as a security change rather than
// buried in a diff to the transport adapter.
//
// The core fact this file is built around is rclone's own default: read
// backend/sftp/sftp.go in the vendored rclone v1.75.0 tree and the default
// case, reached whenever known_hosts_file, pin_host_key, host_keys and the
// ssh option are all unset, is:
//
//	sshConfig.HostKeyCallback = ssh.InsecureIgnoreHostKey()
//
// That accepts any host key from any server, silently (it logs a notice, but
// does not fail, and does not refuse the connection). If this adapter ever
// forwarded an operator's configuration straight through to rclone, an empty
// or missing known_hosts setting would produce exactly that. sftpConfig below
// is the single place that stands between operator configuration and
// rclone's option map, and it never lets that default be reached.
package rclone

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/lib/env"

	"github.com/spdrman/rclone-manager/internal/transport"
)

// sftpConfig turns a transport.Source into the exact rclone sftp backend
// options this adapter is willing to use.
//
// It is deliberately not a pass-through. Every sftp option this function
// does not set is an option this adapter refuses to expose, and that list
// matters as much as the list of what it does set:
//
//   - pass, key_pem, ask_password, key_use_agent are never set, so password
//     authentication and ssh-agent authentication have no path into this
//     adapter. transport.Source has no password field to begin with, so
//     there is nothing upstream that could even be threaded through, but
//     this function is the backstop: even a future caller that adds a
//     Password field to Source cannot reach a password login without also
//     touching this switch statement.
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

	// FR-6: SSH key authentication by default. This is intentionally
	// mandatory rather than optional. rclone's sftp backend, given no
	// key_file, no key_pem, no pass and no ask_password, does not refuse to
	// connect: it falls back to asking a running ssh-agent for a key. That
	// is a real, working authentication path, just not the one this adapter
	// is meant to offer, and an operator who forgot to configure a key would
	// otherwise authenticate against whatever key their agent happens to
	// hold, silently and non-reproducibly. Requiring key_file closes that
	// path by construction.
	if src.KeyFile == "" {
		return nil, fmt.Errorf("source %q: key_file is required for sftp (key-based authentication is mandatory, ssh-agent fallback and password login are not offered)", src.ID)
	}
	keyFilePath := env.ShellExpand(src.KeyFile)
	if _, err := os.Stat(keyFilePath); err != nil {
		return nil, fmt.Errorf("source %q: key_file %q is not accessible: %w", src.ID, src.KeyFile, err)
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
	cfg.Set("key_file", src.KeyFile)
	cfg.Set("known_hosts_file", src.KnownHosts)
	return cfg, nil
}
