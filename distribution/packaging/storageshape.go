package packaging

import (
	"fmt"
	"path"
)

// This file is issue #196's packaging-side check: every canonical storage
// role has one declared write mode, every adapter's mount carries it, and
// the configuration role is a writable DIRECTORY rather than the
// read-only single file it used to be.
//
// Why it is its own checker rather than an assertion inside a test: the
// positive control has to run the same code against a deliberately
// pre-#196 mount, and a control that re-implements the rule it is
// controlling proves nothing about the rule that ships.

const (
	// RuleUndeclaredWriteMode fires for a canonical container path that
	// appears in neither readOnlyContainerPaths nor
	// writableContainerPaths. An undeclared write mode is what let the
	// configuration mount be `:ro` in production and writable in every
	// test fixture at the same time, with nothing in between to notice.
	RuleUndeclaredWriteMode = "undeclared-write-mode"

	// RuleWrongWriteMode fires for a mount whose read-only flag
	// disagrees with the declared mode for its container path.
	RuleWrongWriteMode = "wrong-write-mode"

	// RuleLegacyConfigFileMount fires for the exact pre-#196 shape: a
	// mount at the configuration FILE rather than at the directory that
	// holds it. It is a rule of its own, and not merely "an unknown
	// container path", because that is the shape that silently disabled
	// CreateBackupSet (#146), the settings write path (#140) and #176's
	// first-run setup, and a reader who reintroduces it deserves to be
	// told which three features it breaks rather than that a path is
	// unrecognised.
	//
	// A correction to its own first draft, which is worth keeping: the
	// claim that without this rule "nothing stops it being reintroduced"
	// was wrong. roleMounts's generic "not a container path the canonical
	// image knows about" refusal does stop it. It just says nothing about
	// which three features it breaks, which is the whole reason this rule
	// exists.
	RuleLegacyConfigFileMount = "legacy-config-file-mount"
)

// LegacyConfigContainerPaths is every container path that means "the
// configuration was mounted as a FILE".
//
// Two of them, and the second is the one that matters. The rule used to
// match only ConfigFilePath(), which is the config DIRECTORY plus
// config.yaml, so today it is /etc/backup-manager/config/config.yaml. The
// pre-#196 shape mounted /etc/backup-manager/config.yaml, one level up,
// and that value is not derivable from the current containerPaths.config
// by joining anything to it. So the rule named for the historical shape
// could not fire on the historical shape: a reintroduced
// /etc/backup-manager/config.yaml got Role "" from roleForContainerPath,
// was skipped by CheckStorageShapes's `if m.Role == ""` line, and reached
// only the generic role refusal.
//
// Both are derived rather than written down, so a future move of the
// configuration directory carries them along instead of leaving a
// hardcoded string behind pointing at history.
func LegacyConfigContainerPaths(c Canonical) []string {
	if c.ConfigFileName == "" {
		return nil
	}
	out := []string{c.ConfigFilePath()}
	if beside := path.Join(path.Dir(c.ContainerPaths.Config), c.ConfigFileName); beside != out[0] {
		out = append(out, beside)
	}
	return out
}

// CheckStorageShapes holds one provider's services to the canonical
// storage contract's write modes.
//
// It deliberately does not re-check which host path a role maps to, or
// that every role is present: TestEveryPlatformMapsEveryStorageRoleTheSameWay
// already owns those, and two checks answering one question is how they
// come to disagree.
func CheckStorageShapes(svcs []Service, c Canonical) []Violation {
	var out []Violation

	legacyConfigFiles := LegacyConfigContainerPaths(c)

	for _, svc := range svcs {
		for _, m := range svc.Mounts {
			if contains(legacyConfigFiles, m.ContainerPath) {
				out = append(out, Violation{svc.Source, RuleLegacyConfigFileMount,
					fmt.Sprintf("service %q mounts the configuration FILE at %s. Issue #196: the config role is the directory %s, mounted writable, because CreateBackupSet, the settings write path and first-run setup all replace %s through a temp file created in its own directory, and a single-file mount puts that directory on the image's read-only rootfs",
						svc.Name, m.ContainerPath, c.ContainerPaths.Config, c.ConfigFileName)})
				continue
			}
			if m.Role == "" {
				continue // TestEveryPlatformMapsEveryStorageRoleTheSameWay owns this.
			}
			want := c.WriteModeFor(m.ContainerPath)
			switch want {
			case WriteModeUndeclared:
				out = append(out, Violation{svc.Source, RuleUndeclaredWriteMode,
					fmt.Sprintf("the %q role mounts at %s, which canonical.json lists as neither read-only nor writable, so no rule can decide whether service %q mounting it read-only=%v is right",
						m.Role, m.ContainerPath, svc.Name, m.ReadOnly)})
			case WriteModeReadOnly:
				if !m.ReadOnly {
					out = append(out, Violation{svc.Source, RuleWrongWriteMode,
						fmt.Sprintf("service %q mounts the %q role (%s) writable; canonical.json declares it read-only, and nothing in the container writes it",
							svc.Name, m.Role, m.ContainerPath)})
				}
			case WriteModeWritable:
				if m.ReadOnly {
					out = append(out, Violation{svc.Source, RuleWrongWriteMode,
						fmt.Sprintf("service %q mounts the %q role (%s) read-only; canonical.json declares it writable, and the application creates and atomically replaces what is under it",
							svc.Name, m.Role, m.ContainerPath)})
				}
			}
		}
	}

	sortViolations(out)
	return out
}

// CheckCanonicalWriteModes holds canonical.json itself to the rule that
// every container path it names resolves to exactly one write mode.
//
// Without this the adapter check above is satisfiable by deleting a path
// from both lists, which turns every mount of that role into an
// unanswerable question rather than a failure.
func CheckCanonicalWriteModes(c Canonical) []Violation {
	var out []Violation
	for _, role := range Roles {
		p, ok := c.ContainerPaths.ByRole(role)
		if !ok {
			continue
		}
		switch c.WriteModeFor(p) {
		case WriteModeUndeclared:
			out = append(out, Violation{"canonical.json", RuleUndeclaredWriteMode,
				fmt.Sprintf("the %q role's container path %s is in neither readOnlyContainerPaths nor writableContainerPaths, or in both", role, p)})
		}
	}
	if c.ConfigFileName == "" {
		out = append(out, Violation{"canonical.json", RuleLegacyConfigFileMount,
			"configFileName is empty, so nothing says which file lives in the configuration directory"})
	}
	if path.Ext(c.ContainerPaths.Config) != "" {
		out = append(out, Violation{"canonical.json", RuleLegacyConfigFileMount,
			fmt.Sprintf("the config role's container path %s names a file, not the directory the application owns (issue #196)", c.ContainerPaths.Config)})
	}
	sortViolations(out)
	return out
}
