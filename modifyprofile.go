package main

// Modification profiles — named de-identification recipes ported from the
// dicomtool CLI. dicomqr keeps its own copies of the profile store
// (~/.dicomqr/profiles.json) and the tag alias map (~/.dicomqr/tags.json),
// seeded from embedded defaults on first run and never overwritten, so
// hand-edits survive upgrades. The JSON format and merge semantics match
// dicomtool, letting profiles be copied between the two tools, with three
// deliberate divergences: `zip` and `transfersyntax` are dicomqr-only
// (dicomtool ignores unknown keys), and dicomtool's `maskrows` is intentionally
// unsupported here — a dicomtool-authored value is dropped on load and stripped
// on save.

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/suyashkumar/dicom/pkg/tag"
)

//go:embed defaults/profiles.json
var defaultModProfilesJSON []byte

//go:embed defaults/tags.json
var defaultModTagsJSON []byte

// ModProfile holds a named collection of modification parameters. Fields map
// directly to the equivalent dicomtool modify command-line parameters; the
// JSON keys are identical to dicomtool's Profile so the stores interoperate,
// except: Zip is dicomqr-only (dicomtool's zip is a CLI-run parameter, not a
// profile field), TransferSyntax is dicomqr-only (dicomtool has no equivalent),
// and dicomtool's maskrows has no field here by design.
type ModProfile struct {
	Base             string   `json:"base,omitempty"`
	Sets             []string `json:"set,omitempty"`
	Removes          []string `json:"remove,omitempty"`
	Keep             []string `json:"keep,omitempty"`
	DOB              string   `json:"dob,omitempty"`
	UIDSuffix        string   `json:"uid,omitempty"`
	ShiftDays        string   `json:"shiftdays,omitempty"`
	RemapUIDs        bool     `json:"remapuids,omitempty"`
	Priv             bool     `json:"noprivate,omitempty"`
	KeepPrivate      bool     `json:"keepprivate,omitempty"`
	Dicomdir         bool     `json:"dicomdir,omitempty"`
	Verbose          bool     `json:"verbose,omitempty"`
	Zip              bool     `json:"zip,omitempty"`
	IgnoreTypes      []string `json:"ignoretype,omitempty"`
	IgnoreModalities []string `json:"ignoremodality,omitempty"`
	FixVR            string   `json:"fixvr,omitempty"`

	// TransferSyntax is the syntax every exported file is written in, using the
	// same tokens as ServerProfile.TransferSyntax: tsPrefAny (empty) writes each
	// file in the syntax it is stored in, tsPrefExplicitLE or tsPrefImplicitLE
	// convert it. Compressed pixel data is decompressed on the way out; there
	// are no encoders, so a compressed syntax can never be a target.
	//
	// This is independent of the server profile's requirement, which constrains
	// what a retrieve is allowed to receive. Requiring nothing there and setting
	// a syntax here keeps the download folder in the archive's own encoding and
	// converts only on export.
	TransferSyntax string `json:"transfersyntax,omitempty"`

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

// TagConfig maps user-defined shortcut phrases to DICOM tag strings ("GGGG,EEEE").
type TagConfig map[string]string

// Resolve returns the tag string for phrase if it exists in the config,
// otherwise returns phrase unchanged.
func (c TagConfig) Resolve(phrase string) string {
	if c == nil {
		return phrase
	}
	if t, ok := c[phrase]; ok {
		return t
	}
	return phrase
}

// modifyProfilesPath returns ~/.dicomqr/profiles.json.
func modifyProfilesPath() (string, error) {
	dir, err := appSettingsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "profiles.json"), nil
}

// modifyTagsPath returns ~/.dicomqr/tags.json.
func modifyTagsPath() (string, error) {
	dir, err := appSettingsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "tags.json"), nil
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

// loadTagConfig reads the tag alias map at path. A missing file returns an
// empty config without error.
func loadTagConfig(path string) (TagConfig, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return TagConfig{}, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg TagConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// ensureDefaultModifyConfigs creates ~/.dicomqr/profiles.json and tags.json
// with compiled-in defaults if they do not already exist. O_EXCL guarantees an
// existing (possibly hand-edited) file is never overwritten.
func ensureDefaultModifyConfigs() {
	dir, err := appSettingsDir()
	if err != nil {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	seed := func(name string, content []byte) {
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return // already exists (or unwritable) — leave it alone
		}
		defer f.Close()
		f.Write(content)
	}
	seed("profiles.json", defaultModProfilesJSON)
	seed("tags.json", defaultModTagsJSON)
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

	result.Priv = base.Priv || override.Priv
	result.Dicomdir = base.Dicomdir || override.Dicomdir
	result.Verbose = base.Verbose || override.Verbose
	result.Zip = base.Zip || override.Zip
	result.RemapUIDs = base.RemapUIDs || override.RemapUIDs

	// Sets: override wins per tag; base contributes tags not in override.
	overrideTags := make(map[string]bool, len(override.Sets))
	for _, s := range override.Sets {
		if t, _, ok := strings.Cut(s, "="); ok {
			overrideTags[strings.ToLower(strings.TrimSpace(t))] = true
		}
	}
	result.Sets = make([]string, 0, len(base.Sets)+len(override.Sets))
	for _, s := range base.Sets {
		if t, _, ok := strings.Cut(s, "="); ok {
			if !overrideTags[strings.ToLower(strings.TrimSpace(t))] {
				result.Sets = append(result.Sets, s)
			}
		}
	}
	result.Sets = append(result.Sets, override.Sets...)

	// Removes: union, deduplicated.
	seen := make(map[string]bool, len(base.Removes)+len(override.Removes))
	result.Removes = nil
	for _, r := range append(base.Removes, override.Removes...) {
		if !seen[r] {
			seen[r] = true
			result.Removes = append(result.Removes, r)
		}
	}

	// Keep: union, deduplicated.
	seenK := make(map[string]bool, len(base.Keep)+len(override.Keep))
	result.Keep = nil
	for _, k := range append(base.Keep, override.Keep...) {
		if !seenK[k] {
			seenK[k] = true
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
			keepSet[strings.ToLower(strings.TrimSpace(k))] = true
		}
		filtered := make([]string, 0, len(result.Removes))
		for _, r := range result.Removes {
			if !keepSet[strings.ToLower(strings.TrimSpace(r))] {
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

// validateUIDSuffix trims s and returns it; only the digits 1-9 are allowed
// (0 would create ".0"-prefixed UID components, which are invalid).
func validateUIDSuffix(s string) (string, error) {
	s = strings.TrimSpace(s)
	for _, c := range s {
		if c < '1' || c > '9' {
			return "", fmt.Errorf("the UID suffix may contain only the digits 1-9")
		}
	}
	return s, nil
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

// tagDisplayName returns a human-readable name for t: the standard dictionary
// name when known, else the alias-map name pointing at t, else "".
func tagDisplayName(t tag.Tag, aliases TagConfig) string {
	if info, err := tag.Find(t); err == nil && info.Name != "" {
		return info.Name
	}
	for name, tagStr := range aliases {
		if at, err := parseTagString(tagStr); err == nil && at == t {
			return name
		}
	}
	return ""
}
