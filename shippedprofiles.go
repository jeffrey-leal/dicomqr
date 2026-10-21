package main

// Shipped profiles — the modification profiles in defaults/profiles.json and
// the Tag Profiles in defaults/settings.json. Both stores are seeded from them
// once, on first run, and seeding never touches them again, so an improvement a
// later release makes to a shipped profile reaches an existing install only
// through here: Restore shipped profiles… under each list in Preferences, and
// the window a new version opens at startup when a shipped profile has changed
// since the user was last shown it (checkShippedProfilesAtStartup).
//
// Nothing is updated automatically. A modification profile decides what a
// de-identification removes, and one changing under the user between versions
// is the silent change that retiring base inheritance removed; a Tag Profile's
// colour is the user's own choice. So every restore is chosen row by row, and
// one that replaces something keeps what it replaces:
//   - a modification profile the user has under the same name is kept, renamed
//     "<name> (before restore)", directly below the restored one;
//   - a Tag Profile keeps its colour and enabled state and takes the shipped
//     tag list and modalities, which are what a release improves.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image/color"
	"maps"
	"slices"
	"sort"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/widget"
)

type shippedKind int

const (
	shippedModification shippedKind = iota
	shippedTag
)

type shippedStatus int

const (
	shippedMissing shippedStatus = iota // the user has no profile of that name
	shippedSame                         // the user's matches the shipped one
	shippedDiffers                      // the user's has the name but differs
)

// shippedOffer is one shipped profile set against the user's store.
type shippedOffer struct {
	kind   shippedKind
	name   string
	status shippedStatus
	digest string // of the shipped version; what Settings.ShippedProfilesSeen records
	keepAs string // modification profile that differs: the name the user's is kept under
}

func (o shippedOffer) seenKey() string {
	if o.kind == shippedTag {
		return "tag:" + o.name
	}
	return "modification:" + o.name
}

// shippedModProfiles parses the embedded defaults through the store's own
// parse, so a freshly seeded store compares as identical to them.
func shippedModProfiles() ModProfileConfig {
	cfg, _, err := parseModProfileConfig(defaultModProfilesJSON)
	if err != nil {
		logError("shipped modification profiles do not parse: %v", err)
		return nil
	}
	return cfg
}

// shippedTagProfiles returns the Tag Profiles in the embedded default settings.
func shippedTagProfiles() []TagProfile {
	var s struct {
		TagProfiles []TagProfile `json:"tagProfiles"`
	}
	if err := json.Unmarshal(defaultSettingsJSON, &s); err != nil {
		logError("shipped Tag Profiles do not parse: %v", err)
		return nil
	}
	return s.TagProfiles
}

// modProfileDigest identifies what a modification profile does rather than how
// it is written down. Its JSON form is canonicalised first: arrays are sorted
// (removals are a set, Set values' order is presentation only, mask regions
// apply together) and zero values dropped, since an absent key and an empty
// one read back the same. Tag spellings are already canonical on load
// (normalizeModProfile).
func modProfileDigest(p ModProfile) string {
	data, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return ""
	}
	canon, err := json.Marshal(canonicalJSON(v))
	if err != nil {
		return ""
	}
	return shortDigest(canon)
}

// canonicalJSON sorts every array of v by its elements' encodings and drops
// object members holding a zero value. encoding/json already writes object
// keys sorted.
func canonicalJSON(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			e = canonicalJSON(e)
			if isZeroJSON(e) {
				delete(t, k)
				continue
			}
			t[k] = e
		}
		return t
	case []any:
		type keyed struct {
			key string
			v   any
		}
		ks := make([]keyed, len(t))
		for i, e := range t {
			e = canonicalJSON(e)
			b, _ := json.Marshal(e)
			ks[i] = keyed{string(b), e}
		}
		sort.SliceStable(ks, func(i, j int) bool { return ks[i].key < ks[j].key })
		out := make([]any, len(ks))
		for i := range ks {
			out[i] = ks[i].v
		}
		return out
	}
	return v
}

func isZeroJSON(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case bool:
		return !t
	case float64:
		return t == 0
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	}
	return false
}

// tagProfileDigest covers what restoring a Tag Profile replaces — its tags and
// modalities, in any order — and not its colour or enabled state, which a
// restore keeps.
func tagProfileDigest(p TagProfile) string {
	tags := make([]string, len(p.Tags))
	for i, t := range p.Tags {
		tags[i] = fmt.Sprintf("%04X,%04X", t.Group, t.Element)
	}
	sort.Strings(tags)
	mods := normalizeModalities(p.Modalities)
	sort.Strings(mods)
	return shortDigest([]byte(strings.Join(tags, ";") + "|" + strings.Join(mods, ";")))
}

func shortDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// shippedOffersFor sets every shipped profile against the user's stores:
// modification profiles first (alphabetically, as the shipped file is a map),
// then Tag Profiles in their shipped order. modsReadable is false when
// profiles.json could not be read; its profiles are then not offered at all,
// since nothing may be written over a file that did not parse.
func shippedOffersFor(mods ModProfileConfig, modsReadable bool, tags []TagProfile) []shippedOffer {
	var offers []shippedOffer
	if modsReadable {
		shipped := shippedModProfiles()
		for _, name := range slices.Sorted(maps.Keys(shipped)) {
			o := shippedOffer{kind: shippedModification, name: name, digest: modProfileDigest(shipped[name])}
			if cur, ok := mods[name]; !ok {
				o.status = shippedMissing
			} else if modProfileDigest(cur) == o.digest {
				o.status = shippedSame
			} else {
				o.status = shippedDiffers
				o.keepAs = beforeRestoreName(name, mods)
			}
			offers = append(offers, o)
		}
	}
	for _, sp := range shippedTagProfiles() {
		o := shippedOffer{kind: shippedTag, name: sp.Name, digest: tagProfileDigest(sp), status: shippedMissing}
		if i := tagProfileIndex(tags, sp.Name); i >= 0 {
			o.status = shippedDiffers
			if tagProfileDigest(tags[i]) == o.digest {
				o.status = shippedSame
			}
		}
		offers = append(offers, o)
	}
	return offers
}

func tagProfileIndex(profiles []TagProfile, name string) int {
	return slices.IndexFunc(profiles, func(p TagProfile) bool { return p.Name == name })
}

// startupShippedOffers is what the startup window offers: the shipped profiles
// the user does not already match whose shipped version the user has not been
// shown before. A profile deliberately deleted or edited is therefore offered
// again only once a release changes it.
func startupShippedOffers(offers []shippedOffer, seen map[string]string) []shippedOffer {
	var out []shippedOffer
	for _, o := range offers {
		if o.status != shippedSame && seen[o.seenKey()] != o.digest {
			out = append(out, o)
		}
	}
	return out
}

// markShippedSeen records the digest of every offer in seen, reporting whether
// anything changed so an unchanged map is not written back. A key for a kind
// not evaluated this time (an unreadable profiles.json) is left as it was.
func markShippedSeen(seen map[string]string, offers []shippedOffer) (map[string]string, bool) {
	next := maps.Clone(seen)
	if next == nil {
		next = map[string]string{}
	}
	changed := false
	for _, o := range offers {
		if next[o.seenKey()] != o.digest {
			next[o.seenKey()] = o.digest
			changed = true
		}
	}
	return next, changed
}

// beforeRestoreName is the name a replaced modification profile is kept under:
// "<name> (before restore)", numbered if that is taken.
func beforeRestoreName(name string, cfg ModProfileConfig) string {
	cand := name + " (before restore)"
	for i := 2; ; i++ {
		if _, taken := cfg[cand]; !taken {
			return cand
		}
		cand = fmt.Sprintf("%s (before restore %d)", name, i)
	}
}

// restoreShippedModProfiles installs the named shipped profiles into cfg, in
// place, and returns the list order afterwards together with the names
// replaced profiles were kept under. order is the full current order, as
// orderedModProfileNames gives it. A profile cfg already holds under the name,
// if it differs, is kept under beforeRestoreName directly below the restored
// one, which takes its place; a new profile joins the end, as Add's does.
func restoreShippedModProfiles(cfg ModProfileConfig, order, names []string, shipped ModProfileConfig) ([]string, map[string]string) {
	order = slices.Clone(order)
	kept := map[string]string{}
	for _, name := range names {
		sp, ok := shipped[name]
		if !ok {
			continue
		}
		if cur, has := cfg[name]; has {
			if modProfileDigest(cur) == modProfileDigest(sp) {
				continue
			}
			keep := beforeRestoreName(name, cfg)
			cfg[keep] = cur
			kept[name] = keep
			if j := slices.Index(order, name); j >= 0 {
				order = slices.Insert(order, j+1, keep)
			} else {
				order = append(order, keep)
			}
		} else {
			order = append(order, name)
		}
		cfg[name] = cloneModProfile(sp)
	}
	return orderedModProfileNames(cfg, order), kept
}

// restoreShippedTagProfiles returns profiles with the named shipped Tag
// Profiles installed. One the user already has, by name, takes the shipped tags
// and modalities and keeps its colour and enabled state. A missing one is
// inserted ahead of the first of the user's profiles that follows it in the
// shipped order — the first matching profile gives a row its colour
// (matchProfile), and the shipped set relies on PHI coming first — or appended
// when none does.
func restoreShippedTagProfiles(profiles []TagProfile, names []string, shipped []TagProfile) []TagProfile {
	out := slices.Clone(profiles)
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	for si, sp := range shipped {
		if !want[sp.Name] {
			continue
		}
		if i := tagProfileIndex(out, sp.Name); i >= 0 {
			out[i].Tags = slices.Clone(sp.Tags)
			out[i].Modalities = slices.Clone(sp.Modalities)
			continue
		}
		at := len(out)
		for _, later := range shipped[si+1:] {
			if j := tagProfileIndex(out, later.Name); j >= 0 {
				at = j
				break
			}
		}
		np := sp
		np.Tags = slices.Clone(sp.Tags)
		np.Modalities = slices.Clone(sp.Modalities)
		out = slices.Insert(out, at, np)
	}
	return out
}

// shippedOfferNames returns the names of offers of one kind.
func shippedOfferNames(offers []shippedOffer, kind shippedKind) []string {
	var names []string
	for _, o := range offers {
		if o.kind == kind {
			names = append(names, o.name)
		}
	}
	return names
}

// shippedOfferStatusText is the explanation beside an offer's checkbox.
func shippedOfferStatusText(o shippedOffer) string {
	switch {
	case o.status == shippedSame:
		return "Same as yours"
	case o.status == shippedMissing:
		return "Not installed — ticking adds it"
	case o.kind == shippedModification:
		return fmt.Sprintf("Differs from yours — yours is kept as “%s”", o.keepAs)
	}
	return "Tags differ from yours — yours keeps its colour"
}

// shippedOfferStartsTicked: adding a missing profile loses nothing, so it
// starts ticked; replacing one starts unticked, so it is a deliberate choice.
func shippedOfferStartsTicked(o shippedOffer) bool {
	return o.status == shippedMissing
}

// shippedWindowText is the wording that differs between the Preferences and
// the startup use of the window.
type shippedWindowText struct {
	title, intro, note      string
	applyLabel, cancelLabel string
}

// showShippedProfilesWindow lists offers with a checkbox each, grouped by
// kind, and calls onApply with the ticked ones. An offer the user already
// matches is listed disabled, so the whole shipped set is visible. onCancel
// runs when the window closes any other way — Cancel or the title-bar button.
// The window is owned by parent and blocks it.
func showShippedProfilesWindow(a fyne.App, parent fyne.Window, text shippedWindowText, offers []shippedOffer,
	onApply func([]shippedOffer), onCancel func()) {

	var win fyne.Window
	applied := false
	checks := make([]*widget.Check, len(offers))
	applyBtn := widget.NewButton(text.applyLabel, nil)
	applyBtn.Importance = widget.HighImportance
	syncApply := func() {
		for _, c := range checks {
			if c.Checked {
				applyBtn.Enable()
				return
			}
		}
		applyBtn.Disable()
	}

	sections := map[shippedKind][]fyne.CanvasObject{}
	for i, o := range offers {
		// The handler is attached after the initial tick: SetChecked fires it,
		// and syncApply reads every check, the later ones not yet made.
		check := widget.NewCheck(o.name, nil)
		check.SetChecked(shippedOfferStartsTicked(o))
		check.OnChanged = func(bool) { syncApply() }
		if o.status == shippedSame {
			check.Disable()
		}
		checks[i] = check
		status := widget.NewLabel(shippedOfferStatusText(o))
		status.Wrapping = fyne.TextWrapWord
		status.Importance = widget.LowImportance
		sections[o.kind] = append(sections[o.kind], container.NewBorder(nil, nil, check, nil, status))
	}
	syncApply()

	intro := widget.NewLabel(text.intro)
	intro.Wrapping = fyne.TextWrapWord
	parts := []fyne.CanvasObject{intro}
	if rows := sections[shippedModification]; len(rows) > 0 {
		parts = append(parts, prefSection("Modification profiles", rows...))
	}
	if rows := sections[shippedTag]; len(rows) > 0 {
		parts = append(parts, prefSection("Tag Profiles", rows...))
	}
	body := container.NewVScroll(container.New(
		layout.NewCustomPaddedLayout(modEditorMargin, modEditorMargin, modEditorMargin, modEditorMargin),
		container.NewVBox(parts...)))
	body.SetMinSize(fyne.NewSize(0, 220))

	note := widget.NewLabel(text.note)
	note.Wrapping = fyne.TextWrapWord
	note.TextStyle = fyne.TextStyle{Italic: true}
	cancelBtn := widget.NewButton(text.cancelLabel, func() { win.Close() })
	applyBtn.OnTapped = func() {
		var chosen []shippedOffer
		for i, c := range checks {
			if c.Checked && !c.Disabled() {
				chosen = append(chosen, offers[i])
			}
		}
		applied = true
		win.Close()
		onApply(chosen)
	}
	buttonRow := container.NewBorder(widget.NewSeparator(), nil, nil, nil,
		container.New(
			layout.NewCustomPaddedLayout(modEditorMargin, modEditorMargin, modEditorMargin, modEditorMargin),
			container.NewBorder(nil, nil, nil, container.NewHBox(cancelBtn, applyBtn), note)))
	minWidth := canvas.NewRectangle(color.Transparent)
	minWidth.SetMinSize(fyne.NewSize(520, 0))

	openOwnedWindow(a, windowSpec{
		Key:      "shipped-profiles",
		Title:    text.title,
		Size:     fyne.NewSize(620, 480),
		Place:    "shipped-profiles",
		Parent:   parent,
		Blocking: true,
		OnClosed: func() {
			if !applied && onCancel != nil {
				onCancel()
			}
		},
	}, func(w fyne.Window) fyne.CanvasObject {
		win = w
		return container.NewStack(minWidth, container.NewBorder(nil, buttonRow, nil, nil, body))
	})
}

// shippedPreferencesNote is the window's note when opened from Preferences.
const shippedPreferencesNote = "Nothing is saved until you press Apply in Preferences."

// showRestoreShippedFromPreferences opens the window from one of Preferences'
// lists, offering that list's kind only, and hands the chosen offers back to
// be applied to Preferences' pending state.
func showRestoreShippedFromPreferences(a fyne.App, parent fyne.Window, kind shippedKind,
	mods ModProfileConfig, tags []TagProfile, onApply func([]shippedOffer)) {

	var offers []shippedOffer
	for _, o := range shippedOffersFor(mods, kind == shippedModification, tags) {
		if o.kind == kind {
			offers = append(offers, o)
		}
	}
	intro := "The modification profiles supplied with this version of dicomqr. Tick the ones to restore; " +
		"a profile of yours with the same name is kept, renamed, below the restored one."
	if kind == shippedTag {
		intro = "The Tag Profiles supplied with this version of dicomqr. Tick the ones to restore; " +
			"a profile of yours with the same name takes the supplied tags and modalities and keeps its colour."
	}
	if len(offers) == 0 {
		dialog.ShowInformation("Restore shipped profiles", "This version supplies no profiles of this kind.", parent)
		return
	}
	showShippedProfilesWindow(a, parent, shippedWindowText{
		title:       "Restore Shipped Profiles",
		intro:       intro,
		note:        shippedPreferencesNote,
		applyLabel:  "Restore selected",
		cancelLabel: "Cancel",
	}, offers, onApply, nil)
}

// checkShippedProfilesAtStartup compares the shipped profiles with the user's
// stores once the main window is up. When a shipped profile the user does not
// already match has changed since the user was last shown it, it opens the
// window over the main window so the user can update; either answer records
// what was shown, and so does a start with nothing to show. cfg is the
// session's settings; onTagProfilesChanged runs after a restore changed them.
// Must run on the UI goroutine.
func checkShippedProfilesAtStartup(a fyne.App, parent fyne.Window, cfg *Settings, onTagProfilesChanged func()) {
	var mods ModProfileConfig
	path, modsErr := modifyProfilesPath()
	if modsErr == nil {
		mods, modsErr = loadModProfileConfig(path)
	}
	offers := shippedOffersFor(mods, modsErr == nil, cfg.TagProfiles)
	record := func() {
		if seen, changed := markShippedSeen(cfg.ShippedProfilesSeen, offers); changed {
			cfg.ShippedProfilesSeen = seen
			saveSettings(*cfg)
		}
	}
	pending := startupShippedOffers(offers, cfg.ShippedProfilesSeen)
	if len(pending) == 0 {
		record()
		return
	}
	names := make([]string, len(pending))
	for i, o := range pending {
		names[i] = o.name
	}
	logInfo("shipped profiles: this version supplies new or changed profiles: %s", strings.Join(names, ", "))

	showShippedProfilesWindow(a, parent, shippedWindowText{
		title: "Updated Default Profiles",
		intro: "This version of dicomqr supplies profiles that are new or have changed since you last saw them, " +
			"and that differ from yours. Tick the ones to install. Nothing of yours is lost: a modification " +
			"profile you have under the same name is kept, renamed, below the restored one, and a Tag Profile " +
			"keeps your colour.",
		note: "Not now keeps your profiles as they are and does not ask again until a later version changes them. " +
			"Restore shipped profiles… in Preferences offers them at any time.",
		applyLabel:  "Update selected",
		cancelLabel: "Not now",
	}, pending, func(chosen []shippedOffer) {
		cfg.ShippedProfilesSeen, _ = markShippedSeen(cfg.ShippedProfilesSeen, offers)
		applyShippedAtStartup(parent, cfg, chosen, onTagProfilesChanged) // saves settings
	}, record)
}

// applyShippedAtStartup writes the chosen offers straight to the stores — the
// startup window has no Preferences behind it to stage them in — and logs what
// it did. profiles.json is read again rather than reused: it is written only
// from a fresh, successful read.
func applyShippedAtStartup(parent fyne.Window, cfg *Settings, chosen []shippedOffer, onTagProfilesChanged func()) {
	if names := shippedOfferNames(chosen, shippedModification); len(names) > 0 {
		path, err := modifyProfilesPath()
		var mods ModProfileConfig
		if err == nil {
			mods, err = loadModProfileConfig(path)
		}
		if err == nil {
			order := orderedModProfileNames(mods, cfg.ModProfileOrder)
			var kept map[string]string
			order, kept = restoreShippedModProfiles(mods, order, names, shippedModProfiles())
			if err = saveModProfileConfig(path, mods); err == nil {
				cfg.ModProfileOrder = storedModProfileOrder(order)
				for _, n := range names {
					if keep, ok := kept[n]; ok {
						logInfo("shipped profiles: restored modification profile %q; yours is kept as %q", n, keep)
					} else {
						logInfo("shipped profiles: installed modification profile %q", n)
					}
				}
			}
		}
		if err != nil {
			dialog.ShowError(fmt.Errorf("the modification profiles could not be updated:\n\n%v", err), parent)
		}
	}
	if names := shippedOfferNames(chosen, shippedTag); len(names) > 0 {
		cfg.TagProfiles = restoreShippedTagProfiles(cfg.TagProfiles, names, shippedTagProfiles())
		logInfo("shipped profiles: restored Tag Profile(s) %s", strings.Join(names, ", "))
		if onTagProfilesChanged != nil {
			onTagProfilesChanged()
		}
	}
	if err := saveSettingsE(*cfg); err != nil {
		dialog.ShowError(fmt.Errorf("the updated settings could not be saved to disk:\n\n%v", err), parent)
	}
}
