package compose_test

import (
	"sync/atomic"
	"testing"

	"github.com/spdrman/rclone-manager/distribution/compose"
)

// separation_test.go is issue #87 (B5.1)'s state-separation regression
// suite: "private state, credentials and host keys are proven separate
// from backup data on EVERY claimed platform".
//
// contract_test.go's TestPrivateStateAndBackupDataAreSeparateMounts
// already proves it, and proves it well, for the canonical definition.
// The canonical definition is not a claimed platform, it is the thing
// claimed platforms derive from, and a derivation is exactly where a
// mount gets re-pointed at whatever host path that NAS OS happens to hand
// out. This runs the same rule over every derived artifact the contract
// registers, so an adapter cannot land a layout that nests the local-auth
// record or the SSH private key inside a share an operator hands to a
// user.
//
// The rule is written as "whatever an artifact DOES declare must not nest
// with the backup destination", not as "every artifact must declare all of
// these". Requiring the full field set of a derived artifact would make it
// a second definition rather than a derivation, which is the asymmetry
// runtime-contract.json is explicit about.

// privatePaths are the container paths that must never live inside the
// backup destination. Each one holds something an operator would not
// knowingly publish: the lifecycle journal and the local-auth Argon2id
// record, the manager's configuration, the SFTP private key, and the
// pinned host keys that are the whole of this product's MITM defence.
var privatePaths = []struct {
	containerPath string
	what          string
}{
	{"/data/state", "the lifecycle journal and the local-auth administrator record"},
	{"/etc/backup-manager/config.yaml", "the manager's configuration"},
	{"/etc/backup-manager/id_ed25519", "the SFTP private key"},
	{"/etc/backup-manager/known_hosts", "the pinned host keys"},
}

const backupDataPath = "/data/backups"

// separationEnv is contract_test.go's env() plus every other variable a
// registered artifact interpolates into a host path.
//
// Supplying them is not cosmetic. compose.Document.Mounts splits a
// short-syntax volume on ":", so an unexpanded ${DISK:?set DISK in ...},
// whose default-message half contains colons of its own, parses into a
// host path that matches no container path at all, and every mount rule
// keyed on a container path then finds nothing and reports nothing. That
// is a silent fail-open, which is why the suite below FAILS rather than
// skips when an artifact yields no backup destination.
func separationEnv() map[string]string {
	out := map[string]string{
		"DISK":     "/srv/dev-disk-by-uuid-0000",
		"KEY_FILE": "/srv/backup-manager/secrets/id_ed25519",
	}
	for k, v := range env() {
		out[k] = v
	}
	return out
}

func TestPrivateStateIsSeparateFromBackupDataOnEveryClaimedPlatform(t *testing.T) {
	t.Parallel()

	c := compose.MustLoadContract()
	artifacts := append([]string{c.Canonical}, c.Derived...)
	if len(artifacts) < 2 {
		t.Fatalf("the contract registers %d artifact(s); this suite proves nothing about derived adapters below two", len(artifacts))
	}

	var checked atomic.Int64
	for _, rel := range artifacts {
		t.Run(rel, func(t *testing.T) {
			t.Parallel()

			doc, err := compose.Read(compose.Path(rel), separationEnv())
			if err != nil {
				t.Fatalf("read %s: %v", rel, err)
			}
			backups, ok := doc.MountFor(compose.RoleEngine, backupDataPath)
			if !ok {
				t.Fatalf("%s yields no engine mount at %s, so every rule below silently checks nothing on this artifact.\n"+
					"Either the adapter genuinely does not mount backup data, or its volume line did not parse (an unexpanded ${VAR:?message} splits on the colons in its own message)",
					rel, backupDataPath)
			}

			for _, p := range privatePaths {
				mount, declared := doc.MountFor(compose.RoleEngine, p.containerPath)
				if !declared {
					continue
				}
				checked.Add(1)
				if mount.HostPath == backups.HostPath {
					t.Errorf("%s: %s and the backup destination share the host path %q", p.what, p.containerPath, mount.HostPath)
					continue
				}
				if compose.Contains(backups.HostPath, mount.HostPath) {
					t.Errorf("%s lives at %q, inside the backup destination %q: %s",
						p.what, mount.HostPath, backups.HostPath, p.containerPath)
				}
				if compose.Contains(mount.HostPath, backups.HostPath) {
					t.Errorf("the backup destination %q lives inside %q, which holds %s",
						backups.HostPath, mount.HostPath, p.what)
				}
			}
		})
	}

	t.Cleanup(func() {
		if checked.Load() == 0 {
			t.Error("no artifact declared any private mount alongside a backup destination, so this suite compared nothing")
		}
	})
}

// TestTheSeparationRuleWouldNoticeANestedLayout is the positive control.
// Without it, a Contains that never reported containment (or a MountFor
// that never found a mount) would make the suite above pass on a layout
// that puts the SSH private key inside the backup share.
func TestTheSeparationRuleWouldNoticeANestedLayout(t *testing.T) {
	t.Parallel()

	const nested = `
services:
  engine:
    image: backup-manager:dev
    command: ["/backup-manager-web", "serve", "--profile=generic"]
    volumes:
      - /srv/backups:/data/backups
      - /srv/backups/private:/data/state
      - /srv/backups/keys/id_ed25519:/etc/backup-manager/id_ed25519:ro
`
	doc, err := compose.Parse([]byte(nested), "synthetic-nested.yaml", separationEnv())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	backups, ok := doc.MountFor(compose.RoleEngine, backupDataPath)
	if !ok {
		t.Fatal("the synthetic document declares no backup mount, so this control proves nothing")
	}

	for _, containerPath := range []string{"/data/state", "/etc/backup-manager/id_ed25519"} {
		mount, declared := doc.MountFor(compose.RoleEngine, containerPath)
		if !declared {
			t.Fatalf("the synthetic document declares no mount at %s", containerPath)
		}
		if !compose.Contains(backups.HostPath, mount.HostPath) {
			t.Errorf("a mount at %q inside the backup destination %q was not reported as nested, so the rule above fails open",
				mount.HostPath, backups.HostPath)
		}
	}
}
