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
// is vendored INTO and a marker string those files have to carry, and the
// artifacts it is recorded IN and which have to name the licence, its
// text and whose work it is. Both are read. And the sweep runs the other
// way too: a file under the UI's source that carries a third-party
// attribution and that no entry claims is refused, so vendoring
// something else without declaring it is a red build rather than a
// discrepancy nobody looks for.
package packaging

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
	// VendoredInto names the source files that carry the material, and
	// Marker is the string each of them has to contain. Together they are
	// what stops this entry describing last year's artwork: upgrade the
	// paths without touching the marker and the files disagree with the
	// register, loudly.
	VendoredInto []string `json:"vendoredInto"`
	Marker       string   `json:"marker"`
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
	case strings.TrimSpace(v.Marker) == "":
		return "names no marker for those files to carry, so an upgrade of the material could not be told from a match"
	case len(v.RecordedIn) == 0:
		return "names no artifact that carries the attribution, so the obligation is written down and nothing discharges it"
	}
	return ""
}

// Attributes reports whether one artifact's body carries everything this
// entry's attribution needs, and says what is missing when it does not.
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
var vendoredAttributionMarkers = []string{
	"CC BY 4.0",
	"CC-BY-4.0",
	"creativecommons.org/licenses",
	"Font Awesome",
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
var vendoredScanRoots = []string{"ui/shared/src", "ui/shared/index.html"}

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
		if why := v.incompleteBecause(); why != "" {
			out = append(out, fmt.Sprintf("the vendored asset %q %s", name, why))
			continue
		}

		for _, rel := range v.VendoredInto {
			claimed[filepath.ToSlash(rel)] = name
			data, err := read(rel)
			if err != nil {
				out = append(out, fmt.Sprintf("%s says it is vendored into %s, which is not in the tree: %v", name, rel, err))
				continue
			}
			if !strings.Contains(string(data), v.Marker) {
				out = append(out, fmt.Sprintf("%s says %s carries %q and it does not; either the material was upgraded without this register following it, or this register names a file that stopped carrying it", name, rel, v.Marker))
			}
		}

		for _, rel := range v.RecordedIn {
			// A file that carries the attribution is claimed by carrying
			// it. Without this the sweep below would report index.html,
			// which names the licence precisely because it is one of the
			// artifacts recording it.
			claimed[filepath.ToSlash(rel)] = name
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
	for _, root := range vendoredScanRoots {
		abs := Path(root)
		if _, err := os.Stat(abs); err != nil {
			out = append(out, fmt.Sprintf("the vendored-material sweep is rooted at %s, which is not in the tree: %v; a sweep over nothing reports nothing and passes", root, err))
			continue
		}
		err := filepath.WalkDir(abs, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "node_modules" || d.Name() == "dist" {
					return filepath.SkipDir
				}
				return nil
			}
			switch filepath.Ext(path) {
			case ".ts", ".tsx", ".css", ".html":
			default:
				return nil
			}
			rel, relErr := filepath.Rel(Path(""), path)
			if relErr != nil {
				return relErr
			}
			rel = filepath.ToSlash(rel)
			// A test is not shipped material. It is allowed to name a
			// licence in order to assert something about one, which is
			// exactly what icon-artwork.test.tsx does.
			if strings.Contains(rel, ".test.") || strings.Contains(rel, "/test/") {
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
			out = append(out, fmt.Sprintf("the vendored-material sweep of %s could not complete: %v", root, err))
		}
	}
	sort.Strings(out)
	return out
}
