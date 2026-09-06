package packaging

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// §4A's boundary made executable: a provider package deploys into the
// platform, it does not reconfigure the platform.
//
// That line is the one this project cannot afford to cross by accident.
// Everything else packaging checks is about the deployment being correct;
// this is about an install not changing a machine somebody else's data
// lives on. The failure it guards against is not malice, it is a
// plausible fix: the adapter needs a directory to exist, or a daemon to
// notice a config, and one systemd unit or one omv-salt call in a
// template is a small, reasonable-looking way to get it.
//
// So the check is a fingerprint list rather than a judgement, and the
// list is kept narrow deliberately. hostPlaneMarker's own doc has the
// admission rule and the specific marker that was rejected for failing
// it; read that before adding one.

// RuleHostPlaneModification is what fires when a provider package reaches
// into the host platform's own management plane rather than staying
// inside the deployment it was given.
const RuleHostPlaneModification = "host-management-plane-modification"

// hostPlaneMarker is one unambiguous fingerprint of that.
//
// "Unambiguous" is doing real work in that sentence. The obvious markers
// to reach for — `apt-get install`, `curl | sh` — are useless here: the
// OpenMediaVault profile's own README correctly tells an operator to
// `apt-get install openmediavault-compose`, which is OMV's own Compose
// plugin and a documented prerequisite, and no regular expression can
// tell that apart from installing our software onto the host. A marker
// that has to be argued about is a marker that gets switched off, so this
// list only holds things that have no innocent reading inside a provider
// package:
//
//   - a write into a platform's cluster/config database;
//   - a change to a platform's own web assets or private web API;
//   - a restart or reconfiguration of a platform's own daemons;
//   - a plugin artifact for a platform whose plugin §4A defers;
//   - a host systemd unit or cron entry, which is how "just a small
//     helper service" arrives.
//
// Note what is deliberately absent: using a platform's documented package
// API is not a violation. A DSM package living under /var/packages, an
// Unraid Docker template copied into templates-user, a PVE guest created
// with pct or qm — those are the supported surface, and using it is the
// whole point.
type hostPlaneMarker struct {
	re     *regexp.Regexp
	detail string
}

// hostPlaneMarkers is the list itself. Anything added here has to survive
// the "no innocent reading" test in hostPlaneMarker's doc, and the detail
// string has to say what the match MEANS: a failure that prints only the
// matched text sends the reader looking for a string instead of for the
// host change it stands for.
var hostPlaneMarkers = []hostPlaneMarker{
	{regexp.MustCompile(`/etc/pve\b`), "writes into the Proxmox VE cluster configuration filesystem"},
	{regexp.MustCompile(`/usr/share/pve-manager\b`), "touches the Proxmox VE Web UI's own assets"},
	{regexp.MustCompile(`pvemanagerlib\.js`), "patches the Proxmox VE Web UI bundle, which §4A defers indefinitely"},
	{regexp.MustCompile(`\b(pveproxy|pvedaemon|pvestatd|pve-cluster)\b`), "reconfigures or restarts a Proxmox VE management daemon"},
	{regexp.MustCompile(`/etc/openmediavault\b`), "writes into the OpenMediaVault configuration database"},
	{regexp.MustCompile(`\b(omv-salt|omv-confdbadm|omv-mkconf)\b`), "drives OpenMediaVault's own configuration tooling"},
	{regexp.MustCompile(`\.plg\b`), "ships an Unraid plugin, which v1 does not require"},
	{regexp.MustCompile(`\bmidclt\b`), "drives the TrueNAS middleware client to change host configuration"},
	{regexp.MustCompile(`/usr/syno/synoman\b`), "touches DSM's own web root"},
	{regexp.MustCompile(`\bsynowebapi\b`), "calls DSM's private web API"},
	{regexp.MustCompile(`/etc/systemd/system\b`), "installs a systemd unit onto the host"},
	{regexp.MustCompile(`\bsystemctl\s+(enable|disable|mask|restart|daemon-reload)\b`), "changes host service state"},
	{regexp.MustCompile(`/etc/cron|\bcrontab\b`), "installs a host cron entry"},
	{regexp.MustCompile(`\bpatch\s+-p[0-9]`), "applies a source patch, which inside a provider package means patching the host"},
}

// fencedBlockRe matches a fenced code block in Markdown, including the
// fences.
var fencedBlockRe = regexp.MustCompile("(?s)```.*?```")

// ScanForHostPlaneModification reports every way the tree under root
// reaches into the host platform's management plane.
//
// Markdown is scanned differently from everything else, and the
// difference is the rule rather than a convenience. Prose that says "this
// profile never writes into the cluster configuration filesystem" is the
// documentation §4A asks for; a fenced command block that does write
// there is an instruction an operator will paste. So for a .md file only
// the fenced blocks are scanned, and for every other text file the whole
// content is, comments included — a comment is exactly where a "run this
// on the host first" would live.
func ScanForHostPlaneModification(root string) ([]Violation, error) {
	return scanText(root, func(rel, text string) []Violation {
		region := text
		if strings.EqualFold(filepath.Ext(rel), ".md") {
			region = strings.Join(fencedBlockRe.FindAllString(text, -1), "\n")
			if region == "" {
				return nil
			}
		}
		var out []Violation
		for _, m := range hostPlaneMarkers {
			if loc := m.re.FindString(region); loc != "" {
				out = append(out, Violation{rel, RuleHostPlaneModification,
					fmt.Sprintf("%s (%s)", m.detail, backquote(strings.TrimSpace(loc)))})
			}
		}
		return out
	})
}
