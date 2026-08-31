package spk

// This file is the documented Synology package layout, transcribed once
// so every other file in the package (and every test) reads it from one
// place rather than repeating string literals.
//
// Source: Synology's Package Developer Guide, "Synology Package"
// section — the .spk structure tree, the INFO necessary fields, package
// .tgz, the conf directory, and the DSM desktop-application config file.
// The outer archive's own format is taken from pkgscripts-ng's
// include/pkg_util.sh (`pkg_make_spk` runs `tar cf`, uncompressed).

const (
	// PackageName is INFO's `package` key: the package identity DSM keys
	// everything else off, including /var/packages/<name>.
	PackageName = "BackupManager"

	// DisplayName is what Package Center and the DSM desktop show.
	DisplayName = "Backup Manager"

	// Maintainer is INFO's `maintainer` key.
	Maintainer = "spdrman"

	// OSMinVer is INFO's `os_min_ver`. 7.0-40314 rather than a rounder
	// 7.0-40000 because this package keeps its state in
	// /var/packages/<pkg>/var, which Synology's package FHS documents as
	// available from exactly that build.
	OSMinVer = "7.0-40314"

	// Description is INFO's `description`, shown in Package Center.
	Description = "Pull-based backup manager for SFTP sources, with retention, verification and a local web UI."

	// UIPort is the LAN-facing port the shared Web UI is served on, and
	// INFO's `adminport`. The engine is NOT published: it binds
	// EnginePort on loopback and is reached only through the UI host's
	// reverse proxy, matching container/compose.yaml's two-service
	// topology exactly rather than inventing a Synology-only one.
	UIPort = 8477

	// EnginePort is the loopback-only port the engine listens on.
	EnginePort = 8478
)

const (
	// INFOName, PayloadName, ScriptsDir, ConfDir and the two icons are
	// the members of the .spk archive itself.
	INFOName    = "INFO"
	PayloadName = "package.tgz"
	ScriptsDir  = "scripts"
	ConfDir     = "conf"
	IconName    = "PACKAGE_ICON.PNG"
	Icon256Name = "PACKAGE_ICON_256.PNG"

	// PayloadBinDir is where the two release binaries live inside
	// package.tgz. DSM extracts that archive to
	// /var/packages/<pkg>/target, so "bin/backup-manager" lands at
	// /var/packages/BackupManager/target/bin/backup-manager.
	//
	// Member names carry no "./" prefix, matching what the toolkit's own
	// pkg_make_inner_tarball produces: it pipes `ls <dir>` into `tar -C
	// <dir> -T -`, so every name is relative with no leading dot.
	PayloadBinDir = "bin"

	// PayloadShareDir holds files the lifecycle scripts read at runtime,
	// currently just the starter configuration postinst seeds from.
	PayloadShareDir = "share"

	// DSMUIDir is INFO's `dsmuidir`: the directory inside the target that
	// DSM exposes at /webman/3rdparty/<name>/ and reads the desktop
	// launcher's `config` out of.
	DSMUIDir = "ui"

	// DSMAppName is INFO's `dsmappname`, and the key of the launcher
	// entry in ui/config.
	DSMAppName = "SYNO.SDS._ThirdParty.App." + PackageName
)

const (
	// PkgVarPath and PkgEtcPath are the two documented FHS directories
	// that survive both an upgrade and an uninstall. Synology documents
	// no environment variable for either (script_env_var.html lists
	// SYNOPKG_PKGDEST but no SYNOPKG_PKGVAR/SYNOPKG_PKGETC), so the
	// lifecycle scripts spell the documented paths out instead of relying
	// on a variable that may not be exported.
	PkgVarPath = "/var/packages/${SYNOPKG_PKGNAME}/var"
	PkgEtcPath = "/var/packages/${SYNOPKG_PKGNAME}/etc"

	// DataShareName is the DSM shared folder the package asks the
	// data-share resource worker to create. Synology documents that such
	// a share "will not be removed after package uninstallation, since it
	// might delete the user's personal data as well" — which is the
	// mechanism issue #85's retained-backup-safety criterion rests on.
	DataShareName = "backup-manager"
)

// CoreBinaries are the two provider-neutral executables the canonical
// release produces and this package wraps unchanged. They are also the
// keys container/release-manifest.json records a SHA-256 under.
var CoreBinaries = []string{"backup-manager", "backup-manager-web"}

// LifecycleScriptNames is the full set of lifecycle scripts the documented
// structure allows. This package ships all of them, including the ones
// whose body is deliberately a no-op, so that "this stage does nothing"
// is written down and reviewable rather than inferred from an absence.
var LifecycleScriptNames = []string{
	"preinst", "postinst",
	"preuninst", "postuninst",
	"preupgrade", "postupgrade",
	"start-stop-status",
}

// RequiredSPKMembers is every path inside the .spk that must be present.
// Synology's structure page marks INFO, package.tgz, scripts, conf and
// the two icons as mandatory; the individual conf and scripts entries
// below are the ones this package actually relies on.
var RequiredSPKMembers = func() []string {
	members := []string{INFOName, PayloadName, ConfDir + "/privilege", ConfDir + "/resource", IconName, Icon256Name}
	for _, s := range LifecycleScriptNames {
		members = append(members, ScriptsDir+"/"+s)
	}
	return members
}()

// NecessaryINFOFields are the six keys Synology's "Necessary Fields" page
// lists. Every one of them has to be present and non-empty for Package
// Center to show a sane entry.
var NecessaryINFOFields = []string{"package", "version", "os_min_ver", "description", "arch", "maintainer"}
