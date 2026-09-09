package packaging

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The third category of third-party material: what this repository
// carries in its own source (issue #621).
//
// provenance/third-party-licenses.json is derived, and that is its whole
// claim: every Go component is a module `go list -deps` reports as linked
// into a shipped binary, and every npm component is a non-dev entry of
// ui/shared/package-lock.json. Neither of those can see a third kind of
// thing, and #621 put one in the tree. The web UI draws its icons as
// Font Awesome Free SVG paths compiled into the bundle, vendored into
// this project's own source rather than resolved through a package
// manager, and they are CC BY 4.0, which is an attribution licence.
//
// So NOTICE had nowhere to record an obligation this project genuinely
// carries. Not because anybody decided not to: because the generator
// reads a module graph and a lockfile, and vendored artwork is in
// neither. That is the gap this file closes.
//
// #631 put a second kind of thing in the same gap and this register took
// it rather than growing a sibling. The web UI's typeface used to come
// from a font service over the network, which is an empty page on a NAS
// with no route off its LAN, so IBM Plex now ships as woff2 files served
// out of the image. Those are OFL-1.1, an attribution licence with a
// reserved font name, and they are in no lockfile either. Two registers
// would have meant two NOTICE sections and two sets of rules for one
// obligation, so there is one, and the two kinds are told apart by
// carriedAs rather than by which file they are declared in.
//
// # Why a register rather than an inventory entry
//
// The obvious alternative was to put it in the inventory as a component
// with a third ecosystem, which would have carried it into NOTICE and
// into the SBOM for free. It is refused for the reason the inventory's
// own note gives about itself: every row in that file is re-derived from
// the tree on every run, and TestThirdPartyInventoryMatchesTheLiveModuleGraph
// fails on any difference. A row that could only ever be DECLARED would
// have to be exempted from that comparison, and an inventory with one
// hand-written row in it is an inventory a reader can no longer take at
// face value.
//
// Keeping the two apart costs one thing and it is worth naming: the SPDX
// SBOM is built from the inventory, so it does not carry the vendored
// artwork either. That is a real gap and not a decided one. It is
// separate work because an SPDX package for vendored source wants a
// download location, a supplier and a file-level checksum that this
// register does not carry yet, and inventing three fields to make one
// entry appear would be the same mistake in the other direction.
//
// # What makes this a check rather than a declaration
//
// A register that only records is a register that goes stale the first
// time somebody upgrades the artwork. So every entry names the files it
// is vendored INTO and how those files are held to it, and the artifacts
// it is recorded IN and which have to name the licence, its text and
// whose work it is. Both are read. And the sweep runs the other way too:
// a file under the UI's source that carries a third-party attribution and
// that no entry claims is refused, so vendoring something else without
// declaring it is a red build rather than a discrepancy nobody looks for.
//
// # Why carriedAs decides how a file is held
//
// The two kinds of material cannot be checked the same way and the
// difference is not a detail.
//
// Material carried as SOURCE is reproduced inside a file this project
// writes: the icon paths live in a component that changes whenever the
// component changes. A digest over that file would go stale on the next
// edit, and a check nobody can satisfy gets deleted rather than fixed, so
// source is held to a MARKER, a string the file has to keep carrying.
//
// Material carried as FILES is the upstream's own bytes, redistributed.
// A marker is useless there, because a woff2 is compressed and carries no
// searchable string, and it would be the wrong question anyway. OFL-1.1
// section 3 reserves the font name for the copyright holder's own builds,
// so "these are IBM's bytes and not somebody's re-subsetting" is the
// entire permission this product ships the typeface under. That is held
// to a SHA-256 per file, re-derived on every run, because a re-encoded
// copy dropped in beside an unchanged record would look identical in
// review.
//
// Each kind is refused for carrying the other's evidence rather than
// merely not needing it. A digest on source and a marker on redistributed
// bytes are both checks that cannot fail, and a check that cannot fail is
// worse than none: it reads like coverage.

// The two ways vendored material reaches a recipient. They are checked
// in opposite directions, and the file comment above says why.
const (
	// VendoredCarriedAsSource: the material is reproduced inside a file
	// this project writes and maintains, and is held to a marker.
	VendoredCarriedAsSource = "source"
	// VendoredCarriedAsFiles: the upstream's own files, redistributed
	// byte for byte, and held to a digest per file.
	VendoredCarriedAsFiles = "files"
)

// VendoredAsset is one piece of third-party material this repository
// carries inside its own source.
//
// The fields are the ones CC BY 4.0 section 3(a)(1) asks an attribution
// to carry, plus the two that make the entry checkable. 3(a)(1) wants the
// creator identified, the copyright notice retained, the licence
// identified with a URI to its text, and a statement of whether the
// material was modified. An SPDX id on its own is none of that, which is
// why this is a struct and not a string.
type VendoredAsset struct {
	// ID is how a complaint names this entry. Not shown to a recipient.
	ID string `json:"id"`
	// Name and Version identify the exact upload somebody read. A later
	// release is different material with its own artwork and its own
	// notice, so the version is part of the attribution and not
	// bookkeeping.
	Name    string `json:"name"`
	Version string `json:"version"`
	// Creator and Copyright are 3(a)(1)(A) and (B).
	Creator   string `json:"creator"`
	Copyright string `json:"copyright"`
	// SPDXID, LicenceName and LicenceTextURL are 3(a)(1)(C) and (D). The
	// URL is the half an id cannot carry: a recipient told "CC-BY-4.0"
	// and not where to read it has been named a licence, not given one.
	SPDXID         string `json:"spdxId"`
	LicenceName    string `json:"licenceName"`
	LicenceTextURL string `json:"licenceTextUrl"`
	// Modified and ModifiedNote are 3(a)(1)(B)'s modification statement.
	// The note is a sentence rather than a rendered boolean because it is
	// the string the artifacts are checked for: "no" is not an
	// attribution, and a phrase both NOTICE and the record have to carry
	// is what stops one of them quietly saying something else.
	Modified     bool   `json:"modified"`
	ModifiedNote string `json:"modifiedNote"`
	// WhatShips says which part of the upstream material is actually in
	// the product. Font Awesome Free is icons, fonts and code under three
	// different licences, and only the icons are here, so a record that
	// did not say which part would be claiming obligations this project
	// does not have and hiding the one it does.
	WhatShips string `json:"whatShips"`
	// CarriedAs is VendoredCarriedAsSource or VendoredCarriedAsFiles,
	// and it decides which of Marker and Digests holds the files to this
	// entry. Neither is optional under the kind that uses it and both are
	// refused under the kind that does not, because a check that cannot
	// fail reads like coverage.
	CarriedAs string `json:"carriedAs"`
	// VendoredInto names the files in this repository that carry the
	// material. For source that is the component the material was
	// reproduced into; for files it is the redistributed files
	// themselves.
	VendoredInto []string `json:"vendoredInto"`
	// Marker is the string every VendoredInto file has to contain, for
	// material carried as source. It is what stops this entry describing
	// last year's artwork: upgrade the paths without touching the marker
	// and the files disagree with the register, loudly.
	Marker string `json:"marker"`
	// Digests is path to SHA-256, for material carried as files. It has
	// to name exactly the same set as VendoredInto: a digest for a path
	// this entry does not ship checks a file nobody receives, and a path
	// with no digest is the one that would drift.
	Digests map[string]string `json:"digests"`
	// LicenceFile is this repository's own copy of the licence text,
	// carried where the material is. Required for material carried as
	// files, because that is what OFL-1.1 section 2 asks for in as many
	// words: each copy of the Font Software contains the copyright notice
	// and the licence, and the copies include the ones inside the image.
	// CC BY 4.0 asks the other way, for a URI rather than the text, which
	// is why the artwork carries none and is not refused for it.
	LicenceFile string `json:"licenceFile"`
	// LinkedInto names the shipped artifacts the material reaches, in the
	// same words the inventory uses for a component, so a reader meets
	// one vocabulary.
	LinkedInto []string `json:"linkedInto"`
	// SourceURL is where the upstream release this was taken from is
	// served. Not an obligation under CC BY 4.0, which asks for
	// attribution rather than for source, and recorded anyway: it is what
	// lets somebody check the paths against the artwork they claim to be.
	SourceURL string `json:"sourceUrl"`
	// RecordedIn names the artifacts that carry the attribution, and they
	// are read rather than believed. NOTICE belongs here because NOTICE
	// is where a recipient of a built artifact looks.
	RecordedIn []string `json:"recordedIn"`
	Rationale  []string `json:"rationale"`
}

// incompleteBecause says why an entry is not an attribution, or "".
//
// The shape follows AcceptedNonPermissiveLicence's own check, and for the
// same reason it gives: a category whose entries can be half-filled is a
// category that admits anything, and the way to stop that is to make the
// missing field the error rather than the thing a reader has to notice.
func (v VendoredAsset) incompleteBecause() string {
	switch {
	case strings.TrimSpace(v.Name) == "":
		return "names no material, so it attributes nothing in particular"
	case strings.TrimSpace(v.Version) == "":
		return "names no version, so nobody can tell which release's artwork is in the product"
	case strings.TrimSpace(v.Creator) == "":
		return "names no creator, which is the first thing an attribution licence asks for"
	case strings.TrimSpace(v.Copyright) == "":
		return "carries no copyright notice, which CC-style attribution asks to be retained rather than summarised"
	case strings.TrimSpace(v.SPDXID) == "":
		return "names no licence, so a recipient is told whose work it is and not what they may do with it"
	case LicenseExpressionIsUndecided(v.SPDXID):
		return fmt.Sprintf("gives the licence as %q, which is an expression rather than a decided licence; a choice between licences is not a statement of the one this material is under", v.SPDXID)
	case strings.TrimSpace(v.LicenceTextURL) == "":
		return "says nowhere a recipient can read the licence text, and an id nobody can look up is a name rather than terms"
	case strings.TrimSpace(v.ModifiedNote) == "":
		return "makes no statement about modification, which an attribution licence asks for whether or not anything was changed"
	case strings.TrimSpace(v.WhatShips) == "":
		return "does not say which part of the upstream material ships, so it claims either too much or too little"
	case len(v.VendoredInto) == 0:
		return "names no file it is vendored into, so nothing in the tree can be checked against it"
	case v.CarriedAs != VendoredCarriedAsSource && v.CarriedAs != VendoredCarriedAsFiles:
		return fmt.Sprintf("gives carriedAs as %q, which is neither %q nor %q, and nothing else says whether the files it names are held to a marker or to a digest", v.CarriedAs, VendoredCarriedAsSource, VendoredCarriedAsFiles)
	case v.CarriedAs == VendoredCarriedAsSource && strings.TrimSpace(v.Marker) == "":
		return "is carried as source and names no marker for those files to carry, so an upgrade of the material could not be told from a match"
	case v.CarriedAs == VendoredCarriedAsSource && len(v.Digests) > 0:
		return "is carried as source and records a digest; this project maintains those files, so the digest goes stale on the next edit and a check nobody can satisfy gets deleted rather than fixed"
	case v.CarriedAs == VendoredCarriedAsFiles && len(v.Digests) == 0:
		return "redistributes the upstream's own files and records no digest for them, so nothing would notice one being re-encoded, re-subsetted or replaced, which is the whole permission this material ships under"
	case v.CarriedAs == VendoredCarriedAsFiles && strings.TrimSpace(v.Marker) != "":
		return "redistributes the upstream's own files and names a marker for them; a compressed binary carries no searchable string, so that check could never fail and would read like coverage"
	case v.CarriedAs == VendoredCarriedAsFiles && strings.TrimSpace(v.LicenceFile) == "":
		return "redistributes the upstream's own files and ships no copy of the licence text beside them, which is what OFL-1.1 section 2 and its neighbours ask for in every copy"
	case len(v.RecordedIn) == 0:
		return "names no artifact that carries the attribution, so the obligation is written down and nothing discharges it"
	}
	return ""
}

// missingFrom returns the parts of this entry's attribution that one
// artifact's body does not carry, and an empty slice when it carries all
// of them.
//
// A list rather than a bool, and the name says which way round it reads,
// because a caller reporting "this artifact is not an attribution" leaves
// somebody diffing two files to find out which string went.
func (v VendoredAsset) missingFrom(body string) []string {
	var out []string
	for _, want := range []struct{ what, text string }{
		{"the material and its version", v.Name + " " + v.Version},
		{"the creator", v.Creator},
		{"the copyright notice", v.Copyright},
		{"the licence", v.SPDXID},
		{"the address of the licence text", v.LicenceTextURL},
		{"the modification statement", v.ModifiedNote},
	} {
		if strings.TrimSpace(want.text) == "" {
			continue
		}
		if !strings.Contains(body, want.text) {
			out = append(out, fmt.Sprintf("%s (%q)", want.what, want.text))
		}
	}
	return out
}

// vendoredAttributionMarkers are the strings that say a source file is
// carrying somebody else's licensed material.
//
// Deliberately about the LICENCE and not about Font Awesome. A scan
// keyed to the one asset in the register today would go green the moment
// somebody vendored a different one, which is precisely the case the
// other direction of this check exists for.
//
// The list started as the artwork's licence and nothing else, and that
// was a hole rather than a scope: #631 vendored OFL-1.1 font files, whose
// files say "SIL Open Font License" and "OFL-1.1" and none of the four
// strings below as they first stood, so this sweep would have watched
// them arrive undeclared and reported nothing. A sweep keyed to the
// licences already in the register can only ever catch a second copy of
// what is already caught. So the generic SPDX form is here too, which is
// the one string a file carrying somebody else's terms is most likely to
// have whatever those terms are.
var vendoredAttributionMarkers = []string{
	"SPDX-License-Identifier:",
	"CC BY 4.0",
	"CC-BY-4.0",
	"creativecommons.org/licenses",
	"Font Awesome",
	"SIL Open Font License",
	"OFL-1.1",
	"openfontlicense.org",
	"scripts.sil.org/OFL",
	"Reserved Font Name",
}

// vendoredScanRoots are the paths swept for undeclared material.
//
// The UI's source and the page it is served in, and nothing else, which
// is a real limit and is stated rather than left to be discovered: this
// is where vendored artwork lands in this project, because it is the only
// part of the product that draws anything. Go code takes its third-party
// material through modules and the inventory already sees all of it.
//
// index.html is here as well as src/ because it is the one file a
// recipient of the built artifact actually receives with an attribution
// in it, and a sweep that read the source and not the served page would
// have missed exactly the copy that matters most.
// A pattern with a "*" in it is expanded, and an expansion that matches
// nothing is a complaint: apps/*/frontend was left out of the first
// version of this list and is where every provider shell lives, so half
// the code that draws anything was unswept while the sweep read as
// complete.
var vendoredScanRoots = []string{
	"ui/shared/src",
	"ui/shared/index.html",
	"ui/shared/public",
	"apps/*/frontend",
}

// vendoredScanExtensions are the files the marker sweep reads.
//
// .txt is here for the licence texts that travel with redistributed
// files. Those are the upstream's own words, so they carry every marker
// in the list above, and a sweep that skipped them would skip the one
// file whose presence is the licence condition.
var vendoredScanExtensions = map[string]bool{
	".ts": true, ".tsx": true, ".css": true, ".html": true, ".txt": true,
}

// vendoredCarriedFileRoots are directories where EVERY file has to be
// claimed, whatever it contains.
//
// The marker sweep asks whether a file says it carries somebody else's
// material, and a woff2 cannot: it is compressed, so it holds no
// searchable string and the sweep reads straight past it. That is the
// exact shape of the failure this register exists to stop, so redistributed
// binaries get the stronger rule instead. Drop a second typeface in here
// and the build goes red for the file rather than for anything it happens
// to contain.
var vendoredCarriedFileRoots = []string{"ui/shared/public/fonts"}

// VendoredAssetComplaints says every way the vendored-asset register and
// the tree disagree.
//
// It reads the tree, which is what makes it a check. Three directions,
// and dropping any one of them leaves a hole somebody would walk into:
//
//   - a declaration that is not an attribution at all (incompleteBecause);
//   - a declaration the tree does not bear out, either because the file
//     it names is gone or because that file no longer carries the marker,
//     which is what an artwork upgrade looks like from here;
//   - material in the tree that no declaration claims, which is what
//     vendoring something new and forgetting the paperwork looks like.
//
// The NOTICE arm of the second direction is not an independent proof and
// should not be read as one: NOTICE is rendered by buildNotice from this
// same register, and TestComplianceArtifactsMatchThisTree keeps the
// checked-in file byte-identical to that render, so on a data change it
// cannot fail. It earns its place the way the same arm does for the
// MPL-2.0 offer: it catches a renderer that stops emitting a string a
// recipient needs. The hand-written record beside it is the artifact
// that can disagree with the register.
func VendoredAssetComplaints(c Compliance, read ReadFileFunc) []string {
	if read == nil {
		return []string{"no reader was supplied to the vendored-asset check, so no file was read and nothing was checked; an unread artifact is not a discharged obligation"}
	}

	var out []string
	claimed := map[string]string{}

	for _, v := range c.License.VendoredAssets {
		name := v.ID
		if strings.TrimSpace(name) == "" {
			name = v.Name
		}

		// A file this entry names is claimed by being named, and that
		// happens BEFORE the completeness check rather than after it.
		// An entry missing one field used to report that field and then
		// every file it covers as undeclared material, so one accurate
		// complaint arrived under several that would go away on their
		// own the moment it was fixed. The sweep's question is whether
		// anybody has declared this file, and somebody has.
		for _, rel := range v.VendoredInto {
			claimed[filepath.ToSlash(rel)] = name
		}
		for _, rel := range v.RecordedIn {
			// A file that carries the attribution is claimed by carrying
			// it. Without this the sweep below would report index.html,
			// which names the licence precisely because it is one of the
			// artifacts recording it.
			claimed[filepath.ToSlash(rel)] = name
		}
		if strings.TrimSpace(v.LicenceFile) != "" {
			claimed[filepath.ToSlash(v.LicenceFile)] = name
		}

		if why := v.incompleteBecause(); why != "" {
			out = append(out, fmt.Sprintf("the vendored asset %q %s", name, why))
			continue
		}

		out = append(out, v.vendoredIntoComplaints(name, read)...)
		out = append(out, v.licenceFileComplaints(name, read)...)

		for _, rel := range v.RecordedIn {
			data, err := read(rel)
			if err != nil {
				out = append(out, fmt.Sprintf("%s's attribution is declared as recorded in %s, which is not in the tree: %v", name, rel, err))
				continue
			}
			for _, missing := range v.missingFrom(string(data)) {
				out = append(out, fmt.Sprintf("%s never carries %s's %s, so it is not the attribution %s asks for", rel, name, missing, v.SPDXID))
			}
		}
	}

	out = append(out, undeclaredVendoredMaterial(claimed)...)
	return out
}

// vendoredIntoComplaints holds the files this entry covers to whichever
// evidence its kind uses. The file comment above argues the split.
func (v VendoredAsset) vendoredIntoComplaints(name string, read ReadFileFunc) []string {
	var out []string
	covered := map[string]bool{}
	for _, rel := range v.VendoredInto {
		covered[rel] = true
		data, err := read(rel)
		if err != nil {
			out = append(out, fmt.Sprintf("%s says it is vendored into %s, which is not in the tree: %v", name, rel, err))
			continue
		}
		if v.CarriedAs == VendoredCarriedAsSource {
			if !strings.Contains(string(data), v.Marker) {
				out = append(out, fmt.Sprintf("%s says %s carries %q and it does not; either the material was upgraded without this register following it, or this register names a file that stopped carrying it", name, rel, v.Marker))
			}
			continue
		}
		want, recorded := v.Digests[rel]
		if !recorded {
			out = append(out, fmt.Sprintf("%s redistributes %s and records no digest for it, so nothing would notice that file being re-encoded, re-subsetted or replaced", name, rel))
			continue
		}
		if got := SHA256Bytes(data); !strings.EqualFold(want, got) {
			out = append(out, fmt.Sprintf("%s records %s as %s and the file in this tree is %s; the whole licensing argument for this material is that it is the upstream's own bytes", name, rel, want, got))
		}
	}
	for rel := range v.Digests {
		if !covered[rel] {
			out = append(out, fmt.Sprintf("%s records a digest for %s, which is not one of the files it says it is vendored into, so it checks a file this entry does not ship", name, rel))
		}
	}
	sort.Strings(out)
	return out
}

// licenceFileComplaints reads this repository's own copy of the licence
// text, for material whose licence asks to travel with every copy.
//
// It is checked for the COPYRIGHT NOTICE rather than for the SPDX id,
// because the id is this project's shorthand and the notice is the
// upstream's own words. An OFL text says "SIL OPEN FONT LICENSE Version
// 1.1" and never the string "OFL-1.1", so asking for the id here would
// refuse the real licence file and accept one that merely quoted us.
func (v VendoredAsset) licenceFileComplaints(name string, read ReadFileFunc) []string {
	if strings.TrimSpace(v.LicenceFile) == "" {
		return nil
	}
	data, err := read(v.LicenceFile)
	if err != nil {
		return []string{fmt.Sprintf("%s ships its licence text at %s and it is not in the tree: %v", name, v.LicenceFile, err)}
	}
	if !strings.Contains(string(data), v.Copyright) {
		return []string{fmt.Sprintf("%s ships %s as the licence that travels with the files and it does not carry the notice %q, which is the half of it that names whose work this is", name, v.LicenceFile, v.Copyright)}
	}
	return nil
}

// undeclaredVendoredMaterial is the sweep that runs the other way.
//
// It walks the UI's source for a file carrying somebody else's licence
// and refuses any that no register entry claims. A file is claimed by
// being named in a vendoredInto, which is the same list the direction
// above reads, so the two cannot drift apart into agreeing with each
// other about different things.
//
// It reads the real tree rather than through the injected reader,
// because the question is which files EXIST and a reader answers only
// about files somebody already knew to ask for. That is the whole point
// of this direction.
func undeclaredVendoredMaterial(claimed map[string]string) []string {
	var out []string
	for _, pattern := range vendoredScanRoots {
		roots, err := expandScanRoot(pattern)
		if err != nil {
			out = append(out, fmt.Sprintf("the vendored-material sweep is rooted at %s and that root could not be resolved: %v; a sweep over nothing reports nothing and passes", pattern, err))
			continue
		}
		for _, abs := range roots {
			out = append(out, sweepForMarkers(abs, claimed)...)
		}
	}
	for _, root := range vendoredCarriedFileRoots {
		out = append(out, sweepForUnclaimedFiles(root, claimed)...)
	}
	sort.Strings(out)
	return out
}

// expandScanRoot resolves one entry of vendoredScanRoots to the absolute
// paths it names, and refuses one that names nothing.
//
// A root that has moved or been renamed is the failure mode worth
// catching: the sweep goes on running, finds nothing where nothing is,
// and reports a clean tree. So an unmatched pattern and a missing path
// are both errors rather than an empty result.
func expandScanRoot(pattern string) ([]string, error) {
	if !strings.Contains(pattern, "*") {
		abs := Path(pattern)
		if _, err := os.Stat(abs); err != nil {
			return nil, err
		}
		return []string{abs}, nil
	}
	matches, err := filepath.Glob(Path(pattern))
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("the pattern matches nothing in this tree")
	}
	sort.Strings(matches)
	return matches, nil
}

// sweepForMarkers walks one root for a text file carrying somebody
// else's licence that no register entry claims.
func sweepForMarkers(abs string, claimed map[string]string) []string {
	var out []string
	err := filepath.WalkDir(abs, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "node_modules" || d.Name() == "dist" || d.Name() == "dist-bundles" {
				return filepath.SkipDir
			}
			return nil
		}
		if !vendoredScanExtensions[filepath.Ext(path)] {
			return nil
		}
		rel, relErr := repoRelative(path)
		if relErr != nil {
			return relErr
		}
		// A test is not shipped material. It is allowed to name a
		// licence in order to assert something about one, which is
		// exactly what icon-artwork.test.tsx and vendored-fonts.test.ts
		// both do.
		if isTestPath(rel) {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		body := string(data)
		for _, marker := range vendoredAttributionMarkers {
			if !strings.Contains(body, marker) {
				continue
			}
			if _, ok := claimed[rel]; !ok {
				out = append(out, fmt.Sprintf("%s carries %q and no vendoredAssets entry in compliance.json claims it; third-party material in this repository's own source is material NOTICE has to attribute, and nothing here can attribute what it does not know about", rel, marker))
			}
			return nil
		}
		return nil
	})
	if err != nil {
		out = append(out, fmt.Sprintf("the vendored-material sweep of %s could not complete: %v", abs, err))
	}
	return out
}

// sweepForUnclaimedFiles walks one root where every file has to be
// claimed, whatever it contains.
func sweepForUnclaimedFiles(root string, claimed map[string]string) []string {
	var out []string
	abs := Path(root)
	entries, err := os.ReadDir(abs)
	if err != nil {
		return []string{fmt.Sprintf("this product redistributes files out of %s and that directory is not in the tree: %v; a sweep over nothing reports nothing and passes", root, err)}
	}
	if len(entries) == 0 {
		return []string{fmt.Sprintf("%s is empty, so the sweep that holds every redistributed file to a register read nothing", root)}
	}
	for _, entry := range entries {
		if entry.IsDir() {
			out = append(out, sweepForUnclaimedFiles(root+"/"+entry.Name(), claimed)...)
			continue
		}
		rel := root + "/" + entry.Name()
		if _, ok := claimed[rel]; !ok {
			// Deliberately the same "compliance.json claims it" phrase
			// the marker sweep uses. Both are the one direction that
			// cannot be satisfied by declaring less, they arrive from
			// the same call, and a caller separating this file's own
			// complaints from the tree's should not have to know there
			// are two sweeps behind them.
			out = append(out, fmt.Sprintf("%s is redistributed to every operator who opens the UI and no vendoredAssets entry in compliance.json claims it; it carries no searchable string for a marker sweep to find, so this is the only rule standing between it and shipping attributed to nobody", rel))
		}
	}
	return out
}

// repoRelative turns an absolute path inside this repository into the
// slash-separated form the register declares paths in.
func repoRelative(path string) (string, error) {
	rel, err := filepath.Rel(Path(""), path)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

// isTestPath reports whether a repository path is a test rather than
// shipped material. "/tests/" as well as "/test/": apps/common/tests is
// a whole workspace of them and the narrower form walked straight past
// it.
func isTestPath(rel string) bool {
	return strings.Contains(rel, ".test.") ||
		strings.Contains(rel, "/test/") ||
		strings.Contains(rel, "/tests/")
}
