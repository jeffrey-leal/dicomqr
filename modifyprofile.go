package main

// Modification profiles — named de-identification recipes ported from the
// dicomtool CLI. dicomqr keeps its own copy of the profile store
// (~/.dicomqr/profiles.json), seeded from embedded defaults on first run and
// never overwritten, so hand-edits survive upgrades.
//
// Every tag reference is stored canonically as zero-padded GGGG,EEEE and
// canonicalised on load, and profiles are merged by comparing parsed tags
// rather than the text that spells them. That matters most for the Keep list,
// which cancels a base profile's Removes: when the comparison was textual, two
// files disagreeing on "40,275" versus "0040,0275" silently stopped a tag being
// preserved.
//
// The JSON format and merge semantics otherwise follow dicomtool, with these
// deliberate divergences: `zip`, `flat`, `transfersyntax`, `nooverlays` and
// `audittags` are dicomqr-only (dicomtool ignores unknown keys); dicomtool's `maskrows` is
// unsupported here, dropped on load and stripped on save; the `uid` suffix
// option is removed — the value is preserved through load and save but a
// profile carrying it is refused at run time in favour of Remap UIDs; and
// dicomqr no longer resolves dicomtool's tag aliases, so a profile copied from
// it must use tag numbers.
// The tag picker names tags from the standard dictionary, which is a better
// naming authority than a map the user has to maintain.

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/suyashkumar/dicom/pkg/tag"
)

//go:embed defaults/profiles.json
var defaultModProfilesJSON []byte

// ModProfile holds a named collection of modification parameters. Fields map
// directly to the equivalent dicomtool modify command-line parameters; the
// JSON keys are identical to dicomtool's Profile so the stores interoperate,
// except: Zip and Flat are dicomqr-only (dicomtool's zip is a CLI-run
// parameter, not a profile field, and has no flat equivalent at all),
// TransferSyntax and IgnoreSOPClasses are dicomqr-only (dicomtool has no
// equivalent for either), and dicomtool's maskrows has no field here by
// design.
type ModProfile struct {
	Base    string   `json:"base,omitempty"`
	Sets    []string `json:"set,omitempty"`
	Removes []string `json:"remove,omitempty"`
	Keep    []string `json:"keep,omitempty"`
	DOB     string   `json:"dob,omitempty"`
	// UIDSuffix is the removed uid-suffix option (Remap UIDs replaced it). The
	// field survives only so a profile still carrying `uid` — dicomtool-authored
	// or old — round-trips through load and save instead of being silently
	// stripped; compileModifyParams refuses to run such a profile, with a
	// message naming the replacement, and the editors disclose the entry.
	UIDSuffix        string   `json:"uid,omitempty"`
	ShiftDays        string   `json:"shiftdays,omitempty"`
	RemapUIDs        bool     `json:"remapuids,omitempty"`
	Priv             bool     `json:"noprivate,omitempty"`
	KeepPrivate      bool     `json:"keepprivate,omitempty"`
	Verbose          bool     `json:"verbose,omitempty"`
	Zip              bool     `json:"zip,omitempty"`
	IgnoreTypes      []string `json:"ignoretype,omitempty"`
	IgnoreModalities []string `json:"ignoremodality,omitempty"`
	// IgnoreSOPClasses skips a file whose SOP Class UID (0008,0016) is listed.
	// The filter for objects that carry the study's imaging modality without
	// being images the modality acquired — scanned documents, dose and
	// protocol pages, saved screens — which arrive as Secondary Capture
	// objects whatever Modality they are labelled with, invisible to the two
	// filters above (see sopclass.go). dicomqr-only. Unlike ignoretype and
	// ignoremodality it is also honored inside a per-modality override, where
	// it adds to the profile's list for that modality alone: a base can skip
	// Secondary Capture for CT, MR, NM and PT and leave an echo study's
	// measurement screens — Secondary Capture too — to the mask review.
	IgnoreSOPClasses []string `json:"ignoresopclass,omitempty"`
	FixVR            string   `json:"fixvr,omitempty"`

	// Dicomdir writes a DICOMDIR (PS3.10 File-set) index alongside the export,
	// referencing every file the run actually wrote — the same key dicomtool
	// uses, but dicomtool's own `dicomdir:true` refuses to combine with
	// `zip:true`; here Zip export and Dicomdir compose freely, the index
	// landing as a "DICOMDIR" entry inside the archive when both are set
	// (dicomdir.go, modifyengine.go). Profile-wide only, like Zip and
	// TransferSyntax — there is no per-modality meaning for a run-level index.
	Dicomdir bool `json:"dicomdir,omitempty"`

	// Flat writes every exported file directly into the export root (or the
	// archive root, with Zip also set) instead of the PHI-safe patient/study/
	// series hierarchy exportLayout otherwise builds — the shape a CD/DVD
	// workflow or a database-free viewer expects. Dropping the folders removes
	// the context a source file name relied on, so a flat export is named
	// after the file's SOP Instance UID instead (the value after Remap UIDs
	// runs, if it does) — see exportLayout.relFor. dicomqr-only, like Zip and
	// Dicomdir; profile-wide for the same reason Dicomdir is.
	Flat bool `json:"flat,omitempty"`

	// NoOverlays removes every overlay-plane group (6000–60FE, even) on export.
	// Overlay Data (60xx,3000) is a bitmap channel a vendor can burn patient
	// text into: no per-tag rule reaches it practically — sixteen repeating
	// groups of it exist — noprivate never touches it (even groups), and pixel
	// masking writes PixelData only. dicomqr-only (dicomtool ignores
	// `nooverlays` and drops it when it saves); profile-wide, like the other
	// scalar options.
	NoOverlays bool `json:"nooverlays,omitempty"`

	// AuditTags marks every exported file as de-identified, as PS3.15 expects
	// of a de-identified object: Patient Identity Removed (0012,0062) = YES, a
	// De-identification Method (0012,0063) naming this application and profile
	// — appended to any method an earlier de-identification recorded, never
	// replacing it — and, when the file's dates were actually shifted,
	// Longitudinal Temporal Information Modified (0028,0303) = MODIFIED. A tag
	// the profile's own Set values write keeps the profile's value. The method
	// text is deliberately not configurable, and no De-identification Method
	// Code Sequence (0012,0064) is written: its codes assert conformance to
	// specific options of the standard's confidentiality profiles, which a
	// user-authored profile may or may not meet. dicomqr-only; profile-wide.
	AuditTags bool `json:"audittags,omitempty"`

	// TransferSyntax is the syntax every exported file is written in, using the
	// same tokens as ServerProfile.TransferSyntax: tsPrefAny (empty) writes each
	// file in the syntax it is stored in, tsPrefExplicitLE or tsPrefImplicitLE
	// convert it. Compressed pixel data is decompressed on the way out; a
	// compressed syntax can never be a target here — the lossless encoders
	// exist only so masking can keep a masked file compressed (its own syntax
	// back, or JPEG 2000 Lossless for a lossy source — recompress.go), never
	// as a conversion a profile can request.
	//
	// This is independent of the server profile's requirement, which constrains
	// what a retrieve is allowed to receive. Requiring nothing there and setting
	// a syntax here keeps the download folder in the archive's own encoding and
	// converts only on export.
	TransferSyntax string `json:"transfersyntax,omitempty"`

	// ExportName pre-fills the Modification dialog's export folder name. It may
	// be a "[GGGG,EEEE]" reference to one of this profile's Set values, which is
	// how the shipped profile names the export after the new patient name — the
	// dialog used to do that from hardcoded knowledge of Patient Name. Empty (or
	// resolving to nothing) leaves the profile-plus-timestamp default. Always
	// only a default: the name is typed per run and never written back.
	ExportName string `json:"exportname,omitempty"`

	// MaskRegions blanks rectangles of burned-in pixels in the exported copy —
	// the patient banner an ultrasound or secondary capture prints into the
	// image itself, which no tag rule can reach. dicomqr-only (dicomtool
	// ignores `maskregions` and drops it when it saves), and the replacement
	// for dicomtool's `maskrows`, which could only blank whole rows from the
	// top of the image.
	//
	// Masking writes pixels, so a compressed source is decompressed on the way
	// out even when the profile asks for no conversion. A losslessly-compressed
	// source (JPEG 2000 Lossless, JPEG Lossless) is then recompressed back into
	// its own syntax, verified bit-identical; a lossy source is re-encoded to
	// JPEG 2000 Lossless — no loss added beyond the decode, where re-entering
	// the lossy syntax would degrade every pixel a second time. Every outcome
	// is reported. See processFile and recompress.go.
	MaskRegions []MaskRegion `json:"maskregions,omitempty"`

	PerModality map[string]ModProfile `json:"per-modality,omitempty"`
}

// modProfileTargetSyntax resolves a profile's TransferSyntax token to the
// transfer syntax UID exports must be written in, mirroring ServerProfile's
// requiredTransferSyntax. An empty token yields ("", true) — write each file as
// stored. ok is false for a token that is neither, which only a hand-edited
// profiles.json can produce: compileModifyParams turns that into a visible
// error rather than silently exporting in the wrong syntax.
func modProfileTargetSyntax(p ModProfile) (string, bool) {
	switch strings.TrimSpace(p.TransferSyntax) {
	case tsPrefAny:
		return "", true
	case tsPrefExplicitLE:
		return tsExplicitVRLE, true
	case tsPrefImplicitLE:
		return tsImplicitVRLE, true
	}
	return "", false
}

// Labels for the TransferSyntax choice, shared by the profile editor and the
// per-run Modification dialog so both name the same thing identically. They
// differ from the server profile's wording on purpose: there the choice governs
// what a retrieve may receive, here it governs what an export is written as.
const (
	tsExportLabelAny      = "As stored (no conversion)"
	tsExportLabelExplicit = "Explicit VR Little Endian (uncompressed)"
	tsExportLabelImplicit = "Implicit VR Little Endian (uncompressed)"
)

// modProfileTSLabels is the option list, in the order the selects present it.
var modProfileTSLabels = []string{tsExportLabelAny, tsExportLabelExplicit, tsExportLabelImplicit}

// transferSyntaxPrefLabel maps a stored token to its label. An unrecognised
// token (only a hand-edited profiles.json can hold one) shows as "as stored",
// which matches nothing the user then saves — compileModifyParams is what
// reports it, so the editor need not.
func transferSyntaxPrefLabel(token string) string {
	switch strings.TrimSpace(token) {
	case tsPrefExplicitLE:
		return tsExportLabelExplicit
	case tsPrefImplicitLE:
		return tsExportLabelImplicit
	}
	return tsExportLabelAny
}

// transferSyntaxPrefFromLabel is the inverse, mapping a selected label back to
// the token stored in the profile.
func transferSyntaxPrefFromLabel(label string) string {
	switch label {
	case tsExportLabelExplicit:
		return tsPrefExplicitLE
	case tsExportLabelImplicit:
		return tsPrefImplicitLE
	}
	return tsPrefAny
}

// ModProfileConfig maps profile names to their definitions.
type ModProfileConfig map[string]ModProfile

// modifyProfilesPath returns ~/.dicomqr/profiles.json.
func modifyProfilesPath() (string, error) {
	dir, err := appSettingsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "profiles.json"), nil
}

// loadModProfileConfig reads the profile store at path. A missing file returns
// an empty config without error.
func loadModProfileConfig(path string) (ModProfileConfig, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ModProfileConfig{}, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg ModProfileConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	// Canonicalise on the way in, so every consumer — editor, engine, merge —
	// sees one spelling per tag and the file the editor writes back holds one
	// too. An entry that does not parse survives untouched for validation.
	for name, p := range cfg {
		cfg[name] = normalizeModProfile(p)
	}
	return cfg, nil
}

// saveModProfileConfig writes cfg to the profile store at path as indented
// JSON, atomically. The JSON keys match dicomtool's Profile, so the saved
// file remains copy-compatible between the two tools. Saving normalizes the
// file's layout (alphabetized profile names, 2-space indent) and drops any
// JSON keys ModProfile does not declare — notably a dicomtool-authored
// `maskrows`, which dicomqr deliberately removed. dicomqr's own `zip` and
// `transfersyntax` keys are written but ignored (and dropped on save) by
// dicomtool.
func saveModProfileConfig(path string, cfg ModProfileConfig) error {
	if cfg == nil {
		cfg = ModProfileConfig{}
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteJSON(path, data)
}

// ensureDefaultModifyConfigs creates ~/.dicomqr/profiles.json with the
// compiled-in defaults if it does not already exist. O_EXCL guarantees an
// existing (possibly hand-edited) file is never overwritten.
//
// A tags.json left behind by a version that had tag aliases is not deleted —
// removing a user's file is worse than leaving an inert one — but nothing reads
// it any more.
func ensureDefaultModifyConfigs() {
	dir, err := appSettingsDir()
	if err != nil {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "profiles.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return // already exists (or unwritable) — leave it alone
	}
	defer f.Close()
	f.Write(defaultModProfilesJSON)
}

// resolveModProfile returns the effective ModProfile for name after fully
// resolving its base chain. Circular references are detected and returned as
// an error.
func resolveModProfile(name string, cfg ModProfileConfig) (ModProfile, error) {
	return resolveModProfileChain(name, cfg, make(map[string]bool))
}

func resolveModProfileChain(name string, cfg ModProfileConfig, visited map[string]bool) (ModProfile, error) {
	if visited[name] {
		return ModProfile{}, fmt.Errorf("circular base reference in profile %q", name)
	}
	visited[name] = true

	p, ok := cfg[name]
	if !ok {
		return ModProfile{}, fmt.Errorf("profile %q not found", name)
	}
	if p.Base == "" {
		return p, nil
	}

	base, err := resolveModProfileChain(p.Base, cfg, visited)
	if err != nil {
		return ModProfile{}, err
	}
	return mergeModProfiles(base, p), nil
}

// mergeModProfiles returns a new ModProfile that represents base with override
// applied on top. Override wins for scalars and integers (when non-zero);
// booleans are OR'd; Sets use per-tag precedence (override wins); Removes are
// a union; override.Keep is subtracted from the merged removal list.
func mergeModProfiles(base, override ModProfile) ModProfile {
	result := base

	if override.DOB != "" {
		result.DOB = override.DOB
	}
	if override.UIDSuffix != "" {
		result.UIDSuffix = override.UIDSuffix
	}
	if override.ShiftDays != "" {
		result.ShiftDays = override.ShiftDays
	}
	if override.FixVR != "" {
		result.FixVR = override.FixVR
	}
	if override.TransferSyntax != "" {
		result.TransferSyntax = override.TransferSyntax
	}
	if override.ExportName != "" {
		result.ExportName = override.ExportName
	}
	// Mask regions replace rather than accumulate. Geometry describes one
	// vendor's screen layout, so a derived profile (or a per-modality block)
	// that states its own regions means "here is where the banner is on these
	// images", not "and also blank whatever the base profile blanked".
	if len(override.MaskRegions) > 0 {
		result.MaskRegions = override.MaskRegions
	}

	result.Priv = base.Priv || override.Priv
	result.NoOverlays = base.NoOverlays || override.NoOverlays
	result.AuditTags = base.AuditTags || override.AuditTags
	result.Dicomdir = base.Dicomdir || override.Dicomdir
	result.Verbose = base.Verbose || override.Verbose
	result.Zip = base.Zip || override.Zip
	result.Flat = base.Flat || override.Flat
	result.RemapUIDs = base.RemapUIDs || override.RemapUIDs

	// Every tag comparison below goes through tagMatchKey, which parses the
	// reference first. Comparing the raw strings would make a profile's Keep
	// list cancel its base's Removes list only when both files happened to spell
	// the tag identically — and a Keep that quietly stops cancelling removes a
	// tag the user asked to preserve.

	// Sets: override wins per tag; base contributes tags not in override.
	overrideTags := make(map[string]bool, len(override.Sets))
	for _, s := range override.Sets {
		if key, ok := setMatchKey(s); ok {
			overrideTags[key] = true
		}
	}
	result.Sets = make([]string, 0, len(base.Sets)+len(override.Sets))
	for _, s := range base.Sets {
		if key, ok := setMatchKey(s); ok && !overrideTags[key] {
			result.Sets = append(result.Sets, s)
		}
	}
	result.Sets = append(result.Sets, override.Sets...)

	// Removes: union, deduplicated.
	seen := make(map[string]bool, len(base.Removes)+len(override.Removes))
	result.Removes = nil
	for _, r := range append(base.Removes, override.Removes...) {
		key := tagMatchKey(r)
		if !seen[key] {
			seen[key] = true
			result.Removes = append(result.Removes, r)
		}
	}

	// Keep: union, deduplicated.
	seenK := make(map[string]bool, len(base.Keep)+len(override.Keep))
	result.Keep = nil
	for _, k := range append(base.Keep, override.Keep...) {
		key := tagMatchKey(k)
		if !seenK[key] {
			seenK[key] = true
			result.Keep = append(result.Keep, k)
		}
	}

	// KeepPrivate: OR.
	result.KeepPrivate = base.KeepPrivate || override.KeepPrivate

	// Apply override.Keep to filter result.Removes: a child profile can restore
	// tags that a parent profile removes.
	if len(override.Keep) > 0 {
		keepSet := make(map[string]bool, len(override.Keep))
		for _, k := range override.Keep {
			keepSet[tagMatchKey(k)] = true
		}
		filtered := make([]string, 0, len(result.Removes))
		for _, r := range result.Removes {
			if !keepSet[tagMatchKey(r)] {
				filtered = append(filtered, r)
			}
		}
		result.Removes = filtered
	}

	// PerModality: merge maps, normalizing keys to uppercase. Override's entries
	// win per key; if both define the same modality, merge them recursively.
	if len(base.PerModality) > 0 || len(override.PerModality) > 0 {
		result.PerModality = make(map[string]ModProfile, len(base.PerModality)+len(override.PerModality))
		for k, v := range base.PerModality {
			result.PerModality[strings.ToUpper(k)] = v
		}
		for k, v := range override.PerModality {
			uk := strings.ToUpper(k)
			if existing, ok := result.PerModality[uk]; ok {
				result.PerModality[uk] = mergeModProfiles(existing, v)
			} else {
				result.PerModality[uk] = v
			}
		}
	}

	// IgnoreTypes: union, deduplicated (case-insensitive).
	seenT := make(map[string]bool, len(base.IgnoreTypes)+len(override.IgnoreTypes))
	result.IgnoreTypes = nil
	for _, v := range append(base.IgnoreTypes, override.IgnoreTypes...) {
		key := strings.ToLower(v)
		if !seenT[key] {
			seenT[key] = true
			result.IgnoreTypes = append(result.IgnoreTypes, v)
		}
	}

	// IgnoreModalities: union, deduplicated (case-insensitive).
	seenM := make(map[string]bool, len(base.IgnoreModalities)+len(override.IgnoreModalities))
	result.IgnoreModalities = nil
	for _, v := range append(base.IgnoreModalities, override.IgnoreModalities...) {
		key := strings.ToLower(v)
		if !seenM[key] {
			seenM[key] = true
			result.IgnoreModalities = append(result.IgnoreModalities, v)
		}
	}

	// IgnoreSOPClasses: union, deduplicated (UIDs compare exactly once trimmed).
	seenS := make(map[string]bool, len(base.IgnoreSOPClasses)+len(override.IgnoreSOPClasses))
	result.IgnoreSOPClasses = nil
	for _, v := range append(base.IgnoreSOPClasses, override.IgnoreSOPClasses...) {
		key := strings.TrimSpace(v)
		if key != "" && !seenS[key] {
			seenS[key] = true
			result.IgnoreSOPClasses = append(result.IgnoreSOPClasses, v)
		}
	}

	result.Base = "" // resolved profile carries no further base reference
	return result
}

// parseTagString converts a "GGGG,EEEE" hex string into a tag.Tag. Leading
// zeros are optional ("8,80" is 0008,0080).
func parseTagString(s string) (tag.Tag, error) {
	parts := strings.SplitN(s, ",", 2)
	if len(parts) != 2 {
		return tag.Tag{}, fmt.Errorf("expected format GGGG,EEEE")
	}
	group, err := strconv.ParseUint(strings.TrimSpace(parts[0]), 16, 16)
	if err != nil {
		return tag.Tag{}, fmt.Errorf("invalid group %q: %w", parts[0], err)
	}
	elem, err := strconv.ParseUint(strings.TrimSpace(parts[1]), 16, 16)
	if err != nil {
		return tag.Tag{}, fmt.Errorf("invalid element %q: %w", parts[1], err)
	}
	return tag.Tag{Group: uint16(group), Element: uint16(elem)}, nil
}

// canonicalTagRef rewrites a tag reference into the single spelling the profile
// store uses: zero-padded uppercase GGGG,EEEE. ok is false when s does not
// parse, and the caller must then keep s verbatim — silently dropping a line
// from a de-identification profile is the one failure this code must not have,
// so an unparsable entry survives to be reported by validation instead.
func canonicalTagRef(s string) (string, bool) {
	t, err := parseTagString(strings.TrimSpace(s))
	if err != nil {
		return strings.TrimSpace(s), false
	}
	return formatTagRef(t), true
}

// canonicalTagRefs canonicalises a tag list, preserving order and any entry
// that does not parse.
func canonicalTagRefs(refs []string) []string {
	if refs == nil {
		return nil
	}
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		ref, _ := canonicalTagRef(r)
		if ref != "" {
			out = append(out, ref)
		}
	}
	return out
}

// canonicalSetRefs canonicalises the tag half of each TAG=VALUE entry, leaving
// the value — which may itself contain "=" — exactly as written.
func canonicalSetRefs(sets []string) []string {
	if sets == nil {
		return nil
	}
	out := make([]string, 0, len(sets))
	for _, s := range sets {
		tagStr, value, ok := strings.Cut(s, "=")
		if !ok {
			out = append(out, strings.TrimSpace(s)) // malformed; validation reports it
			continue
		}
		ref, _ := canonicalTagRef(tagStr)
		out = append(out, ref+"="+value)
	}
	return out
}

// normalizeModProfile returns p with every tag reference canonicalised, at any
// depth. Applied on load so the store holds one spelling per tag, which is what
// lets a profile and the base it inherits from be compared by eye as well as by
// code. mergeModProfiles does not depend on this having run — it compares
// parsed tags — but a canonical file is what the editor writes back.
func normalizeModProfile(p ModProfile) ModProfile {
	p.Sets = canonicalSetRefs(p.Sets)
	p.Removes = canonicalTagRefs(p.Removes)
	p.Keep = canonicalTagRefs(p.Keep)
	if len(p.PerModality) > 0 {
		norm := make(map[string]ModProfile, len(p.PerModality))
		for k, v := range p.PerModality {
			norm[k] = normalizeModProfile(v)
		}
		p.PerModality = norm
	}
	return p
}

// tagMatchKey is the identity two tag references are compared by when merging a
// profile with its base. Parsing first means "40,275", "0040,0275" and
// "0040,275" are one tag rather than three strings, which is what makes a Keep
// list reliably cancel the Removes list it was written against. A reference
// that does not parse falls back to its own text, so unparsable entries still
// deduplicate against themselves instead of collapsing together.
func tagMatchKey(ref string) string {
	if t, err := parseTagString(strings.TrimSpace(ref)); err == nil {
		return formatTagRef(t)
	}
	return strings.ToLower(strings.TrimSpace(ref))
}

// setMatchKey is tagMatchKey for a TAG=VALUE entry, keyed on the tag alone so
// an override replaces the base's value for that tag.
func setMatchKey(entry string) (string, bool) {
	tagStr, _, ok := strings.Cut(entry, "=")
	if !ok {
		return "", false
	}
	return tagMatchKey(tagStr), true
}

// The three scalar profile fields with a validation rule are checked in every
// place they can be edited (profile editor, per-modality sub-editor, the
// per-run Modification dialog), so the rules live here rather than in any one
// dialog. Each validator returns the trimmed value ready for storage.

// validateDOBMask trims s and returns it; a non-empty birth-date mask must be
// exactly 8 characters (YYYYMMDD).
func validateDOBMask(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s != "" && len(s) != 8 {
		return "", fmt.Errorf("the birth date mask must be exactly 8 characters (YYYYMMDD), got %d", len(s))
	}
	return s, nil
}

// origAttrsSeqTag is (0400,0561) Original Attributes Sequence.
var origAttrsSeqTag = tag.Tag{Group: 0x0400, Element: 0x0561}

// modProfileDOBAdvisory returns the warning for a profile that masks the birth
// date but leaves the one sequence that is known to nest a copy of it, or ""
// when there is nothing to say.
//
// The birth-date mask rewrites the top-level (0010,0030) only — unlike removals,
// date shifting, UID remapping and Set values, which all recurse. In ordinary
// image objects that is enough: patient demographics live in the top-level
// Patient Module and the sequences that reference a patient carry identifiers,
// not birth dates. The exception is Original Attributes Sequence, whose nested
// Modified Attributes Sequence (0400,0550) holds the values an earlier
// de-identification changed — so a study processed once before can carry the
// real birth date there while the top-level field reads as masked. Removing
// 0400,0561 takes its nested contents with it, which is why the shipped
// base-deident profile is not exposed.
//
// p must be RESOLVED — base chain merged and Keep applied — because a derived
// profile inherits the removal from its base, and a Keep list can cancel it.
//
// Only the profile-level Removes list is examined. A per-modality override's
// removals apply to that modality alone, so an override carrying this tag would
// still leave every other modality exposed.
func modProfileDOBAdvisory(p ModProfile) string {
	if strings.TrimSpace(p.DOB) == "" {
		return ""
	}
	want := tagMatchKey(formatTagRef(origAttrsSeqTag))
	for _, r := range p.Removes {
		if tagMatchKey(r) == want {
			return ""
		}
	}
	return "This profile masks the birth date but does not remove Original Attributes Sequence " +
		"(0400,0561). A study that has already been de-identified once stores the values that were " +
		"changed inside that sequence, so an original birth date can leave in the export even though " +
		"the top-level field is masked. Add 0400,0561 to Remove tags to close it."
}

// A Set value of exactly "[GGGG,EEEE]" is a reference: it means "whatever this
// same profile sets that tag to". It exists so a profile can state a
// relationship it used to be given — the Modification dialog hardcoded Patient
// Name → Patient ID and kept it alive by guessing whether the field had been
// touched, which was invisible when it went wrong.
//
// A reference reaches only the profile's own Set values, never the value in the
// file being modified. That limit is the whole safety of the feature: in a
// de-identification profile, a reference that could read the source would copy
// the real patient name forward into whatever field named it, once per file and
// with nothing on screen to show it had happened.
//
// The whole value is the reference or none of it is (no substring templating),
// so there is nothing to escape and no ambiguity about a value that happens to
// contain a bracket.
func setValueReference(value string) (tag.Tag, bool) {
	value = strings.TrimSpace(value)
	if len(value) < 3 || value[0] != '[' || value[len(value)-1] != ']' {
		return tag.Tag{}, false
	}
	t, err := parseTagString(value[1 : len(value)-1])
	if err != nil {
		return tag.Tag{}, false
	}
	return t, true
}

// splitSetEntry breaks a "TAG=VALUE" entry into its parsed tag and its value.
// Everything after the first "=" is the value, so a value containing "="
// survives intact.
func splitSetEntry(entry string) (t tag.Tag, value string, ok bool) {
	tagStr, value, hasEq := strings.Cut(entry, "=")
	if !hasEq {
		return tag.Tag{}, "", false
	}
	t, err := parseTagString(strings.TrimSpace(tagStr))
	if err != nil {
		return tag.Tag{}, "", false
	}
	return t, value, true
}

// resolveSetReferences replaces every reference with the value of the entry it
// names, returning the Set list the engine should apply. Entries that are not
// references, and entries whose tag does not parse, pass through untouched —
// the latter are reported by compileModifyParams, which is where an unparsable
// tag belongs.
//
// References resolve one level only: a reference whose target is itself a
// reference is an error rather than a chain to follow, which makes cycles
// impossible by construction instead of something to detect.
func resolveSetReferences(sets []string) ([]string, error) {
	if len(sets) == 0 {
		return sets, nil
	}
	// Index the literal value of every entry, so a reference can be answered
	// without caring where in the list its target sits.
	values := make(map[tag.Tag]string, len(sets))
	for _, s := range sets {
		if t, v, ok := splitSetEntry(s); ok {
			if _, dup := values[t]; !dup {
				values[t] = v
			}
		}
	}

	out := make([]string, 0, len(sets))
	for _, s := range sets {
		t, v, ok := splitSetEntry(s)
		if !ok {
			out = append(out, s)
			continue
		}
		target, isRef := setValueReference(v)
		if !isRef {
			out = append(out, s)
			continue
		}
		if target == t {
			return nil, fmt.Errorf("set value for %s refers to itself", formatTagRef(t))
		}
		targetValue, present := values[target]
		if !present {
			return nil, fmt.Errorf("set value for %s refers to %s, which this profile does not set",
				formatTagRef(t), formatTagRef(target))
		}
		if _, chained := setValueReference(targetValue); chained {
			return nil, fmt.Errorf("set value for %s refers to %s, which is itself a reference — references cannot be chained",
				formatTagRef(t), formatTagRef(target))
		}
		out = append(out, formatTagRef(t)+"="+targetValue)
	}
	return out, nil
}

// setValueFollowers maps each referenced tag to the tags referring to it, so
// the Modification dialog can propagate a value as it is typed. Pure, so the
// relationship is testable without a canvas; the dialog only does the wiring.
func setValueFollowers(sets []string) map[tag.Tag][]tag.Tag {
	var followers map[tag.Tag][]tag.Tag
	for _, s := range sets {
		t, v, ok := splitSetEntry(s)
		if !ok {
			continue
		}
		target, isRef := setValueReference(v)
		if !isRef || target == t {
			continue
		}
		if followers == nil {
			followers = map[tag.Tag][]tag.Tag{}
		}
		followers[target] = append(followers[target], t)
	}
	return followers
}

// maxValueLengths bounds the string VRs whose limit is worth enforcing
// (PS3.5 Table 6.2-1). Only VRs a user plausibly types into a Set value are
// listed; anything absent is accepted at any length.
var maxValueLengths = map[string]int{
	"AE": 16, "AS": 4, "CS": 16, "DS": 16, "IS": 12,
	"LO": 64, "PN": 64, "SH": 16, "UI": 64,
}

// validateSetValue checks a Set value against the value representation of the
// tag it will be written to. It exists because the engine does not: buildElement
// coerces the numeric VRs and rejects a bad number, but every string VR — dates
// included — falls through to a plain string, so setting a DA tag to "ANON"
// used to be accepted by the editor, the dialog and the engine alike, and wrote
// a malformed date into every exported file.
//
// An empty value is always valid: it blanks the element, which is exactly what
// the shipped base-deident profile does with Accession Number. A tag the
// dictionary does not know, or a VR not listed here, accepts anything — this
// check must never be the reason a legitimate edit is refused.
func validateSetValue(t tag.Tag, value string) error {
	if value == "" {
		return nil
	}
	// A reference is a placeholder, not a literal: what it resolves to is what
	// gets checked. Without this, "[0010,0010]" in a date field would be
	// rejected as a malformed date.
	if _, ok := setValueReference(value); ok {
		return nil
	}
	info, err := tag.Find(t)
	if err != nil || len(info.VRs) == 0 {
		return nil
	}
	vr := strings.ToUpper(info.VRs[0])
	name := tagDisplayName(t)
	if name == "" {
		name = formatTagRef(t)
	}
	bad := func(want string) error {
		return fmt.Errorf("%s is a %s value — %s (got %q)", name, vr, want, value)
	}

	if max, ok := maxValueLengths[vr]; ok && len(value) > max {
		return fmt.Errorf("%s is a %s value — at most %d characters (got %d)", name, vr, max, len(value))
	}

	switch vr {
	case "DA":
		if _, err := time.Parse("20060102", value); err != nil {
			return bad("a date as YYYYMMDD")
		}
	case "TM":
		// HHMMSS with optional fractional seconds; hours alone are legal too.
		if !isDICOMTime(value) {
			return bad("a time as HHMMSS or HHMMSS.FFFFFF")
		}
	case "DT":
		if !isDICOMDateTime(value) {
			return bad("a date-time as YYYYMMDDHHMMSS with an optional fraction and UTC offset")
		}
	case "UI":
		if !isDICOMUID(value) {
			return bad("a UID: digits separated by dots")
		}
	case "IS":
		if _, err := strconv.Atoi(strings.TrimSpace(value)); err != nil {
			return bad("a whole number")
		}
	case "DS":
		if _, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err != nil {
			return bad("a decimal number")
		}
	case "US", "UL", "SS", "SL":
		if _, err := strconv.Atoi(strings.TrimSpace(value)); err != nil {
			return bad("a whole number")
		}
	case "FL", "FD":
		if _, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err != nil {
			return bad("a decimal number")
		}
	case "AS":
		// nnnD, nnnW, nnnM or nnnY.
		if len(value) != 4 || !isAllDigits(value[:3]) || !strings.ContainsRune("DWMY", rune(value[3])) {
			return bad("an age as nnnD, nnnW, nnnM or nnnY")
		}
	}
	return nil
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// isDICOMTime accepts HH, HHMM, HHMMSS, optionally with a fractional part.
func isDICOMTime(s string) bool {
	whole, frac, hasFrac := strings.Cut(s, ".")
	if hasFrac && (frac == "" || len(frac) > 6 || !isAllDigits(frac)) {
		return false
	}
	switch len(whole) {
	case 2, 4, 6:
	default:
		return false
	}
	if !isAllDigits(whole) {
		return false
	}
	layouts := map[int]string{2: "15", 4: "1504", 6: "150405"}
	_, err := time.Parse(layouts[len(whole)], whole)
	return err == nil
}

// isDICOMDateTime accepts YYYYMMDD optionally followed by a time, a fraction
// and a ±HHMM UTC offset.
func isDICOMDateTime(s string) bool {
	if i := strings.IndexAny(s, "+-"); i >= 0 {
		off := s[i+1:]
		if len(off) != 4 || !isAllDigits(off) {
			return false
		}
		s = s[:i]
	}
	if len(s) < 8 {
		return false
	}
	if _, err := time.Parse("20060102", s[:8]); err != nil {
		return false
	}
	if len(s) == 8 {
		return true
	}
	return isDICOMTime(s[8:])
}

// isDICOMUID accepts dot-separated numeric components (PS3.5 §9.1).
func isDICOMUID(s string) bool {
	for _, part := range strings.Split(s, ".") {
		if !isAllDigits(part) {
			return false
		}
	}
	return true
}

// validateShiftDays trims s and returns it; a non-empty date shift must parse
// as an integer number of days (sign allowed, and zero is accepted to match
// dicomtool — it is an actionable no-op there).
func validateShiftDays(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if _, err := strconv.Atoi(s); err != nil {
		return "", fmt.Errorf("the date shift must be a whole number of days (e.g. -45)")
	}
	return s, nil
}

// tagDisplayName returns the standard dictionary name for t, or "" for a tag
// the dictionary does not list (the shipped base-deident profile removes three).
// The dictionary is the only naming authority: user-defined aliases were a
// second one, and a tag whose name depended on which of them a user had edited
// was not a name worth showing.
func tagDisplayName(t tag.Tag) string {
	if info, err := tag.Find(t); err == nil {
		return info.Name
	}
	return ""
}
