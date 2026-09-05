// The controls for scan_hostplane.go, which is the one rule in this
// package whose failure is a changed host rather than a wrong package.
//
// Two things are checked that a shorter suite would skip. There is a case
// per marker rather than one case for the function, because a list of
// fourteen patterns where one silently matches nothing is
// indistinguishable, from the outside, from a list of fourteen that all
// work; this package has shipped that exact defect twice. And the
// Markdown handling is checked in both directions, because it is a rule
// and not an exemption: prose describing what the profile does not do has
// to pass, a fenced block that does it has to fail, and a suite that only
// checked the first would let the exemption swallow the second.
package packaging

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestScanForHostPlaneModificationAcceptsACleanPackage is the negative
// control for the controls below: if the baseline were already dirty,
// every positive control would fire for the wrong reason.
func TestScanForHostPlaneModificationAcceptsACleanPackage(t *testing.T) {
	got, err := ScanForHostPlaneModification(cleanFixture(t))
	if err != nil {
		t.Fatalf("ScanForHostPlaneModification: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("clean fixture reported %d violation(s), want 0:\n%s", len(got), format(got))
	}
}

// TestScanForHostPlaneModificationCatchesEveryMarker is the positive
// control WP4.5's second acceptance criterion needs: "PVE host management
// plane is not modified by unsupported UI/plugin hacks" is only a claim
// worth making if the check behind it can fail.
//
// There is one case per marker, on purpose. A single case would prove the
// function can return something and nothing about the other thirteen
// patterns, and a pattern that silently matches nothing is exactly the
// failure mode WP4.3's own controls caught twice (\b does not match
// between "_" and "p", so \bpassword missed ADMIN_PASSWORD).
func TestScanForHostPlaneModificationCatchesEveryMarker(t *testing.T) {
	cases := []struct {
		name string
		file string
		body string
	}{
		{"pve cluster filesystem", "deploy.yml", "x-hook: cp backup-manager.conf /etc/pve/local/\n"},
		{"pve web assets", "deploy.yml", "x-hook: cp panel.js /usr/share/pve-manager/js/\n"},
		{"pve ui bundle patch", "install.yaml", "post: sed -i s/x/y/ pvemanagerlib.js\n"},
		{"pve daemon restart", "install.yaml", "post: systemctl reload pveproxy\n"},
		{"omv config database", "compose/backup-manager.env", "HOOK=/etc/openmediavault/config.xml\n"},
		{"omv tooling", "compose/backup-manager.env", "HOOK=omv-salt deploy run backupmanager\n"},
		{"unraid plugin", "template/backup-manager.xml", "<Plugin>backup-manager.plg</Plugin>\n"},
		{"truenas middleware", "catalog/app.yaml", "hook: midclt call system.general.update\n"},
		{"dsm web root", "conf/resource", "{\"path\": \"/usr/syno/synoman/webman/3rdparty\"}\n"},
		{"dsm private api", "scripts/postinst.yaml", "cmd: synowebapi --exec api=SYNO.Core.Service\n"},
		{"host systemd unit", "deploy.yml", "x-unit: /etc/systemd/system/backup-manager.service\n"},
		{"host service state", "deploy.yml", "x-post: systemctl enable backup-manager\n"},
		{"host cron entry", "deploy.yml", "x-post: crontab -l\n"},
		{"source patch", "deploy.yml", "x-post: patch -p1 < ui.diff\n"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := cleanFixture(t)
			mustWrite(t, filepath.Join(root, tc.file), tc.body)

			got, err := ScanForHostPlaneModification(root)
			if err != nil {
				t.Fatalf("ScanForHostPlaneModification: %v", err)
			}
			if len(got) == 0 {
				t.Fatalf("added %q to an otherwise clean package and nothing fired", strings.TrimSpace(tc.body))
			}
			for _, v := range got {
				if v.Rule != RuleHostPlaneModification {
					t.Errorf("fired rule %q, want %q", v.Rule, RuleHostPlaneModification)
				}
			}
		})
	}
}

// TestScanForHostPlaneModificationReadsMarkdownFencesOnly pins the one
// deliberate asymmetry in the scanner, from both sides. Getting it wrong
// in either direction breaks something real: flag the prose and every
// provider README that documents what it does NOT do goes red, so the
// check gets switched off; skip the fences and a pasteable "run this on
// the host" instruction sails through.
func TestScanForHostPlaneModificationReadsMarkdownFencesOnly(t *testing.T) {
	t.Run("prose naming a host path is documentation", func(t *testing.T) {
		root := cleanFixture(t)
		mustWrite(t, filepath.Join(root, "NOTES.md"),
			"This profile never writes into /etc/pve and never restarts pveproxy.\n")

		got, err := ScanForHostPlaneModification(root)
		if err != nil {
			t.Fatalf("ScanForHostPlaneModification: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("prose about what the profile does not do was flagged:\n%s", format(got))
		}
	})

	t.Run("a fenced command block is an instruction", func(t *testing.T) {
		root := cleanFixture(t)
		mustWrite(t, filepath.Join(root, "NOTES.md"),
			"Set it up like this:\n\n```bash\ncp panel.js /usr/share/pve-manager/js/\n```\n")

		got, err := ScanForHostPlaneModification(root)
		if err != nil {
			t.Fatalf("ScanForHostPlaneModification: %v", err)
		}
		if len(got) == 0 {
			t.Fatal("a fenced block that copies a file into the PVE Web UI's assets was not flagged")
		}
	})
}
