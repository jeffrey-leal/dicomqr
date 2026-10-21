package main

import (
	"encoding/json"
	"image/color"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// A store seeded from the shipped defaults reads back as identical to them, so
// a fresh install is never offered its own profiles at startup.
func TestShippedDefaultsCompareAsSame(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profiles.json")
	if err := os.WriteFile(path, defaultModProfilesJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	mods, err := loadModProfileConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	var s Settings
	if err := json.Unmarshal(defaultSettingsJSON, &s); err != nil {
		t.Fatal(err)
	}
	offers := shippedOffersFor(mods, true, s.TagProfiles)
	kinds := map[shippedKind]int{}
	for _, o := range offers {
		kinds[o.kind]++
		if o.status != shippedSame {
			t.Errorf("%s: status %v on a freshly seeded store", o.seenKey(), o.status)
		}
	}
	if kinds[shippedModification] == 0 || kinds[shippedTag] == 0 {
		t.Fatalf("offers by kind = %v; both kinds are shipped", kinds)
	}
	if got := startupShippedOffers(offers, nil); len(got) != 0 {
		t.Errorf("a fresh install is offered %d profile(s) at startup", len(got))
	}

	// Saving the store in dicomqr's own layout changes nothing either.
	if err := saveModProfileConfig(path, mods); err != nil {
		t.Fatal(err)
	}
	again, err := loadModProfileConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range shippedOffersFor(again, true, nil) {
		if o.kind == shippedModification && o.status != shippedSame {
			t.Errorf("%s differs after a save round trip", o.name)
		}
	}
}

// The window itself, built as the startup check builds it: one checkbox per
// offer starting as shippedOfferStartsTicked says, the matching one disabled,
// Apply enabled only while something is ticked and handing back exactly the
// ticked offers, and closing without Apply reported as a cancel. (Building it
// once crashed the application at startup: SetChecked fired the handler
// before every checkbox existed.)
func TestShippedProfilesWindow(t *testing.T) {
	a := test.NewApp()
	parent := a.NewWindow("main")
	offers := []shippedOffer{
		{kind: shippedModification, name: "added", status: shippedMissing},
		{kind: shippedModification, name: "replaced", status: shippedDiffers, keepAs: "replaced (before restore)"},
		{kind: shippedTag, name: "same", status: shippedSame},
	}
	text := shippedWindowText{title: "Shipped", intro: "intro", note: "note", applyLabel: "Update", cancelLabel: "Not now"}
	open := func(onApply func([]shippedOffer), onCancel func()) (fyne.Window, map[string]*widget.Check, *widget.Button) {
		t.Helper()
		showShippedProfilesWindow(a, parent, text, offers, onApply, onCancel)
		windows := a.Driver().AllWindows()
		win := windows[len(windows)-1]
		checks := map[string]*widget.Check{}
		var apply *widget.Button
		walkObjects(t, win.Content(), func(o fyne.CanvasObject) {
			switch w := o.(type) {
			case *widget.Check:
				checks[w.Text] = w
			case *widget.Button:
				if w.Text == text.applyLabel {
					apply = w
				}
			}
		})
		if len(checks) != len(offers) || apply == nil {
			t.Fatalf("window has %d checkbox(es) and apply button %v", len(checks), apply != nil)
		}
		return win, checks, apply
	}

	var chosen []shippedOffer
	cancelled := false
	_, checks, apply := open(func(c []shippedOffer) { chosen = c }, func() { cancelled = true })
	if !checks["added"].Checked || checks["replaced"].Checked || checks["same"].Checked || !checks["same"].Disabled() {
		t.Errorf("initial state: added %v, replaced %v, same %v (disabled %v)", checks["added"].Checked,
			checks["replaced"].Checked, checks["same"].Checked, checks["same"].Disabled())
	}
	if apply.Disabled() {
		t.Errorf("apply disabled with a profile ticked")
	}
	checks["added"].SetChecked(false)
	if !apply.Disabled() {
		t.Errorf("apply enabled with nothing ticked")
	}
	checks["replaced"].SetChecked(true)
	test.Tap(apply)
	if len(chosen) != 1 || chosen[0].name != "replaced" || cancelled {
		t.Errorf("apply handed back %v (cancelled %v), want only replaced", chosen, cancelled)
	}

	chosen, cancelled = nil, false
	win, _, _ := open(func(c []shippedOffer) { chosen = c }, func() { cancelled = true })
	win.Close()
	if !cancelled || chosen != nil {
		t.Errorf("closing without apply: cancelled %v, chosen %v", cancelled, chosen)
	}
}

// Field hook: DICOMQR_SHIPPED_SAMPLE=<folder holding profiles.json and
// settings.json, e.g. %USERPROFILE%\.dicomqr> lists what this build would offer
// that install, and what its startup window would show. Read-only.
func TestSampleShippedOffers(t *testing.T) {
	dir := os.Getenv("DICOMQR_SHIPPED_SAMPLE")
	if dir == "" {
		t.Skip("set DICOMQR_SHIPPED_SAMPLE to a folder holding profiles.json and settings.json")
	}
	mods, modsErr := loadModProfileConfig(filepath.Join(dir, "profiles.json"))
	var s Settings
	if err := json.Unmarshal(defaultSettingsJSON, &s); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	offers := shippedOffersFor(mods, modsErr == nil, s.TagProfiles)
	for _, o := range offers {
		t.Logf("%-28s %s", o.seenKey(), shippedOfferStatusText(o))
	}
	for _, o := range startupShippedOffers(offers, s.ShippedProfilesSeen) {
		t.Logf("startup window offers %s (starts ticked: %v)", o.seenKey(), shippedOfferStartsTicked(o))
	}
}

// The digest identifies what a profile does: list order, tag spelling and
// empty-versus-absent lists do not count; a real change does.
func TestModProfileDigestIgnoresPresentation(t *testing.T) {
	p := normalizeModProfile(ModProfile{
		Sets:      []string{"0010,0010=ANON", "0010,0020=[0010,0010]"},
		Removes:   []string{"0008,0080", "0008,0090"},
		RemapUIDs: true,
	})
	q := normalizeModProfile(ModProfile{
		Sets:        []string{"0010,0020=[0010,0010]", "0010,0010=ANON"},
		Removes:     []string{"8,90", "0008,0080"},
		RemapUIDs:   true,
		IgnoreTypes: []string{},
	})
	if modProfileDigest(p) != modProfileDigest(q) {
		t.Errorf("order, spelling or an empty list changed the digest")
	}
	r := q
	r.Removes = append(slices.Clone(q.Removes), "0008,1010")
	if modProfileDigest(p) == modProfileDigest(r) {
		t.Errorf("an added removal left the digest unchanged")
	}
	r = q
	r.RemapUIDs = false
	if modProfileDigest(p) == modProfileDigest(r) {
		t.Errorf("switching Remap UIDs off left the digest unchanged")
	}
}

func testTagProfile(name string, c color.RGBA, enabled bool, tags ...uint16) TagProfile {
	p := TagProfile{Name: name, Color: c, Enabled: enabled}
	for i := 0; i+1 < len(tags); i += 2 {
		p.Tags = append(p.Tags, tag.Tag{Group: tags[i], Element: tags[i+1]})
	}
	return p
}

func TestShippedOffersStatuses(t *testing.T) {
	shipped := shippedModProfiles()
	edited := cloneModProfile(shipped["base-deident"])
	edited.Removes = append(edited.Removes, "0008,1010")
	mods := ModProfileConfig{"base-deident": edited, "mine": {Removes: []string{"0010,0010"}}}

	tags := shippedTagProfiles()
	if len(tags) < 3 {
		t.Fatalf("expected the shipped Tag Profiles, got %d", len(tags))
	}
	recoloured := tags[0]
	recoloured.Color = color.RGBA{R: 1, G: 2, B: 3, A: 255}
	recoloured.Tags = slices.Clone(recoloured.Tags)
	slices.Reverse(recoloured.Tags) // order alone is not a difference
	trimmed := tags[1]
	trimmed.Tags = trimmed.Tags[:1]
	user := []TagProfile{recoloured, trimmed} // tags[2:] missing

	byKey := map[string]shippedOffer{}
	for _, o := range shippedOffersFor(mods, true, user) {
		byKey[o.seenKey()] = o
	}
	check := func(key string, want shippedStatus, ticked bool) shippedOffer {
		t.Helper()
		o, ok := byKey[key]
		if !ok {
			t.Fatalf("no offer for %s", key)
		}
		if o.status != want {
			t.Errorf("%s: status %v, want %v", key, o.status, want)
		}
		if shippedOfferStartsTicked(o) != ticked {
			t.Errorf("%s: starts ticked = %v, want %v", key, !ticked, ticked)
		}
		return o
	}
	if o := check("modification:base-deident", shippedDiffers, false); o.keepAs != "base-deident (before restore)" {
		t.Errorf("keepAs = %q", o.keepAs)
	}
	check("modification:deident-US", shippedMissing, true)
	check("tag:"+tags[0].Name, shippedSame, false)
	check("tag:"+tags[1].Name, shippedDiffers, false)
	check("tag:"+tags[2].Name, shippedMissing, true)
	if _, ok := byKey["modification:mine"]; ok {
		t.Errorf("a profile of the user's own was offered")
	}

	// profiles.json unreadable: none of its kind is offered.
	for _, o := range shippedOffersFor(nil, false, user) {
		if o.kind == shippedModification {
			t.Errorf("%s offered from an unreadable store", o.name)
		}
	}
}

// At startup only what changed since it was last shown is offered, and
// recording what was shown makes the next start quiet.
func TestStartupOffersOnlyWhatChanged(t *testing.T) {
	offers := []shippedOffer{
		{kind: shippedModification, name: "a", status: shippedMissing, digest: "d1"},
		{kind: shippedModification, name: "b", status: shippedDiffers, digest: "d2"},
		{kind: shippedTag, name: "a", status: shippedDiffers, digest: "d3"},
		{kind: shippedTag, name: "c", status: shippedSame, digest: "d4"},
	}
	seen := map[string]string{"modification:a": "d1", "modification:b": "old", "other:x": "kept"}
	var got []string
	for _, o := range startupShippedOffers(offers, seen) {
		got = append(got, o.seenKey())
	}
	if want := []string{"modification:b", "tag:a"}; !reflect.DeepEqual(got, want) {
		t.Errorf("startup offers %v, want %v", got, want)
	}

	next, changed := markShippedSeen(seen, offers)
	if !changed {
		t.Errorf("new digests reported as no change")
	}
	if seen["modification:b"] != "old" {
		t.Errorf("markShippedSeen wrote through to its input")
	}
	if next["other:x"] != "kept" || next["tag:c"] != "d4" || next["modification:b"] != "d2" {
		t.Errorf("recorded %v", next)
	}
	if len(startupShippedOffers(offers, next)) != 0 {
		t.Errorf("offered again after being recorded")
	}
	if _, changed := markShippedSeen(next, offers); changed {
		t.Errorf("an unchanged record reported as changed")
	}
}

func TestRestoreShippedModProfiles(t *testing.T) {
	shipped := shippedModProfiles()
	edited := cloneModProfile(shipped["base-deident"])
	edited.Removes = append(edited.Removes, "0008,1010")
	mine := ModProfile{Removes: []string{"0010,0010"}}
	cfg := ModProfileConfig{"base-deident": edited, "mine": mine}
	order := []string{"mine", "base-deident"}

	order, kept := restoreShippedModProfiles(cfg, order, []string{"base-deident", "deident-US"}, shipped)
	want := []string{"mine", "base-deident", "base-deident (before restore)", "deident-US"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("order %v, want %v", order, want)
	}
	if kept["base-deident"] != "base-deident (before restore)" {
		t.Errorf("kept = %v", kept)
	}
	for _, n := range []string{"base-deident", "deident-US"} {
		if modProfileDigest(cfg[n]) != modProfileDigest(shipped[n]) {
			t.Errorf("%s is not the shipped profile after restoring", n)
		}
	}
	if !reflect.DeepEqual(cfg["base-deident (before restore)"], edited) || !reflect.DeepEqual(cfg["mine"], mine) {
		t.Errorf("the user's profiles were not kept as they were")
	}

	// The restored profile is a copy: editing it leaves the shipped one alone.
	p := cfg["base-deident"]
	p.Removes[0] = "FFFF,FFFF"
	if shipped["base-deident"].Removes[0] == "FFFF,FFFF" {
		t.Errorf("a restored profile shares its lists with the shipped one")
	}
	cfg["base-deident"] = p

	// Restoring again numbers the next copy; restoring a profile that already
	// matches is a no-op.
	order, _ = restoreShippedModProfiles(cfg, order, []string{"base-deident", "deident-US"}, shipped)
	if _, ok := cfg["base-deident (before restore 2)"]; !ok {
		t.Errorf("second restore did not keep the edited profile as (before restore 2): %v", order)
	}
	if len(cfg) != 5 {
		t.Errorf("store holds %d profiles, want 5 (no copy of an unchanged deident-US)", len(cfg))
	}
}

func TestRestoreShippedTagProfiles(t *testing.T) {
	orange := color.RGBA{R: 255, G: 140, A: 255}
	green := color.RGBA{G: 200, A: 255}
	shipped := []TagProfile{
		testTagProfile("PHI", orange, true, 0x0010, 0x0010, 0x0010, 0x0020),
		testTagProfile("NM", orange, true, 0x0054, 0x0012, 0x0054, 0x0013),
		testTagProfile("PET", orange, true, 0x0054, 0x1001),
	}
	shipped[1].Modalities = []string{"NM"}
	user := []TagProfile{
		testTagProfile("NM", green, false, 0x0054, 0x0012),
		testTagProfile("Mine", green, true, 0x0008, 0x0080),
	}
	got := restoreShippedTagProfiles(user, []string{"PHI", "NM", "PET"}, shipped)

	var names []string
	for _, p := range got {
		names = append(names, p.Name)
	}
	if want := []string{"PHI", "NM", "Mine", "PET"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("order %v, want %v (PHI ahead of the shipped profile after it, PET appended)", names, want)
	}
	nm := got[1]
	if nm.Color != green || nm.Enabled {
		t.Errorf("NM lost the user's colour or enabled state: %v, %v", nm.Color, nm.Enabled)
	}
	if !reflect.DeepEqual(nm.Tags, shipped[1].Tags) || !reflect.DeepEqual(nm.Modalities, []string{"NM"}) {
		t.Errorf("NM did not take the shipped tags and modalities: %v %v", nm.Tags, nm.Modalities)
	}
	if got[0].Color != orange || !got[0].Enabled {
		t.Errorf("an installed profile should arrive as shipped")
	}
	if len(user[0].Tags) != 1 {
		t.Errorf("restoring changed the input slice")
	}
	got[0].Tags[0] = tag.Tag{Group: 0xFFFF}
	if shipped[0].Tags[0].Group == 0xFFFF {
		t.Errorf("an installed profile shares its tag list with the shipped one")
	}
}
