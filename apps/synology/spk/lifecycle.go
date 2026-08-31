package spk

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// footprintPrefixes are the only path roots a shipped script may delete
// inside. Everything here is either the package's own target directory
// (which DSM replaces on upgrade and removes on uninstall anyway), one of
// its documented FHS directories, or a temporary directory the package
// framework itself hands the script.
//
// Deliberately absent: any DSM volume, any shared folder, and any path
// that resolves through a variable this scanner cannot see assigned.
var footprintPrefixes = []string{
	"${SYNOPKG_PKGDEST}", "$SYNOPKG_PKGDEST",
	"${SYNOPKG_PKGVAR}", "$SYNOPKG_PKGVAR",
	"${SYNOPKG_PKGETC}", "$SYNOPKG_PKGETC",
	"${SYNOPKG_PKGINST_TEMP_DIR}", "$SYNOPKG_PKGINST_TEMP_DIR",
	"${SYNOPKG_TEMP_UPGRADE_FOLDER}", "$SYNOPKG_TEMP_UPGRADE_FOLDER",
	"${SYNOPKG_TEMP_LOGFILE}", "$SYNOPKG_TEMP_LOGFILE",
	"/var/packages/${SYNOPKG_PKGNAME}",
}

// deletingCommands are the verbs that remove something from a filesystem.
var deletingCommands = map[string]bool{
	"rm": true, "rmdir": true, "unlink": true, "shred": true,
}

var assignmentRE = regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)

// ScanForUnsafeDeletes reads a shell script and returns one finding per
// deletion whose target is not provably inside the package's own
// footprint.
//
// This is the static half of issue #85's uninstall/retained-data safety
// criterion. The hardware half puts a canary in the backup share and
// diffs the share across an uninstall
// (docs/acceptance/synology-dsm-package-lifecycle.md step 5); this half
// runs on every commit and answers a narrower question that is still
// worth answering: is there any deletion written down here at all whose
// target could be outside the package?
//
// It is deliberately conservative rather than clever. A target it cannot
// resolve to a footprint path is reported, including one hidden behind a
// variable this scanner never saw assigned - "I could not tell" and "it
// is fine" must not produce the same answer in a check about deleting
// somebody's backups.
func ScanForUnsafeDeletes(scriptName, body string) []string {
	// Three of the lifecycle stages source scripts/common.sh, so a
	// deletion written as "${ENGINE_PID}" is only resolvable with that
	// file's assignments in hand. They are merged in first and the
	// script's own assignments win, so a stage that redefines a path is
	// read the way the shell would read it.
	//
	// If common.sh cannot be read the scan still runs, just with fewer
	// resolvable names - which makes it report more, never less.
	vars := map[string]string{}
	if common, err := assetFS.ReadFile("assets/scripts/common.sh"); err == nil {
		vars = shellAssignments(string(common))
	}
	for name, value := range shellAssignments(body) {
		vars[name] = value
	}

	var findings []string
	for i, rawLine := range strings.Split(body, "\n") {
		line := stripShellComment(rawLine)
		for _, cmd := range splitShellCommands(line) {
			fields := shellFields(cmd)
			if len(fields) == 0 {
				continue
			}
			verb := path.Base(fields[0])

			var targets []string
			switch {
			case deletingCommands[verb]:
				targets = operandsOf(fields[1:])
			case verb == "find" && deletesInPlace(fields):
				// find's own path operands come first, before any
				// -predicate, so one pass over the operands catches them.
				targets = operandsOf(fields[1:])
			default:
				continue
			}

			for _, target := range targets {
				resolved := expandShellVars(target, vars)
				if insideFootprint(resolved) {
					continue
				}
				findings = append(findings, fmt.Sprintf(
					"%s:%d: %s deletes %q, which is not provably inside the package footprint (resolved to %q)",
					scriptName, i+1, verb, target, resolved))
			}
		}
	}
	return findings
}

// deletesInPlace reports whether a `find` invocation removes anything.
func deletesInPlace(fields []string) bool {
	for _, f := range fields {
		if f == "-delete" {
			return true
		}
		if f == "-exec" || f == "-execdir" || f == "-ok" {
			// A -exec of anything at all is treated as a deletion
			// candidate rather than parsed: the whole point of this
			// scanner is that it must not talk itself into "that exec
			// probably does not remove anything."
			return true
		}
	}
	return false
}

// insideFootprint reports whether a resolved path is provably under one
// of the package's own directories.
func insideFootprint(target string) bool {
	if target == "" || strings.Contains(target, "..") {
		return false
	}
	for _, prefix := range footprintPrefixes {
		if target == prefix || strings.HasPrefix(target, prefix+"/") {
			return true
		}
	}
	return false
}

// shellAssignments collects simple NAME=value assignments so a target
// written as "${PKG_VAR}/run/engine.pid" can be resolved back to the
// documented FHS path common.sh assigned it from.
func shellAssignments(body string) map[string]string {
	vars := map[string]string{}
	for _, rawLine := range strings.Split(body, "\n") {
		line := stripShellComment(rawLine)
		m := assignmentRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		vars[m[1]] = unquote(strings.TrimSpace(m[2]))
	}
	return vars
}

// expandShellVars substitutes ${NAME}/$NAME from vars, repeatedly, so a
// variable defined in terms of another resolves. Bounded because a
// self-referential assignment must not spin.
func expandShellVars(s string, vars map[string]string) string {
	for range 8 {
		before := s
		for name, value := range vars {
			s = strings.ReplaceAll(s, "${"+name+"}", value)
			s = strings.ReplaceAll(s, "$"+name, value)
		}
		if s == before {
			break
		}
	}
	return s
}

// stripShellComment drops a whole-line comment. A trailing comment after
// real code is left alone on purpose: cutting at the first '#' would
// truncate a legitimate path or a quoted string, and a comment cannot
// make a deletion on the same line safe anyway.
func stripShellComment(line string) string {
	if strings.HasPrefix(strings.TrimSpace(line), "#") {
		return ""
	}
	return line
}

// splitShellCommands breaks a line on the separators that start a new
// command, so `mkdir -p x && rm -rf /y` is seen as two.
func splitShellCommands(line string) []string {
	replacer := strings.NewReplacer("&&", "\x00", "||", "\x00", ";", "\x00", "|", "\x00")
	return strings.Split(replacer.Replace(line), "\x00")
}

// shellFields splits on whitespace and removes quoting, which is enough
// for the deliberately plain shell these scripts are written in.
func shellFields(cmd string) []string {
	fields := strings.Fields(cmd)
	for i := range fields {
		fields[i] = unquote(fields[i])
	}
	return fields
}

// operandsOf drops option flags, leaving the paths a command acts on.
func operandsOf(fields []string) []string {
	var out []string
	for _, f := range fields {
		if f == "" || strings.HasPrefix(f, "-") {
			continue
		}
		out = append(out, f)
	}
	return out
}

func unquote(s string) string {
	for _, q := range []string{`"`, `'`} {
		if len(s) >= 2 && strings.HasPrefix(s, q) && strings.HasSuffix(s, q) {
			return s[1 : len(s)-1]
		}
	}
	return strings.ReplaceAll(strings.ReplaceAll(s, `"`, ""), `'`, "")
}
