package main

import (
	"fmt"
	"image/color"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	catppuccin "github.com/catppuccin/fyne"
	sqweekdialog "github.com/sqweek/dialog"
)

// Theme pack values persisted in Settings.UITheme ("" = the stock Fyne theme).
// Each maps to a base fyne.Theme in themePackBase and a display label in
// themePackLabels.
const (
	themePackDefault   = ""
	themePackAdwaita   = "adwaita"
	themePackLatte     = "catppuccin-latte"
	themePackFrappe    = "catppuccin-frappe"
	themePackMacchiato = "catppuccin-macchiato"
	themePackMocha     = "catppuccin-mocha"
)

// themePackLabels maps pack values to the labels shown in the Preferences
// selector, in display order.
var themePackLabels = []struct{ pack, label string }{
	{themePackDefault, "Default"},
	{themePackAdwaita, "Adwaita"},
	{themePackLatte, "Catppuccin Latte (light)"},
	{themePackFrappe, "Catppuccin Frappé (dark)"},
	{themePackMacchiato, "Catppuccin Macchiato (dark)"},
	{themePackMocha, "Catppuccin Mocha (dark)"},
}

// themePackBase returns the base theme for a pack value. Unknown values (e.g.
// a hand-edited settings.json) fall back to the stock theme.
func themePackBase(pack string) fyne.Theme {
	switch pack {
	case themePackAdwaita:
		return adwaitaTheme{}
	case themePackLatte, themePackFrappe, themePackMacchiato, themePackMocha:
		t := catppuccin.New()
		switch pack {
		case themePackLatte:
			t.SetFlavor(catppuccin.Latte)
		case themePackFrappe:
			t.SetFlavor(catppuccin.Frappe)
		case themePackMacchiato:
			t.SetFlavor(catppuccin.Macchiato)
		case themePackMocha:
			t.SetFlavor(catppuccin.Mocha)
		}
		return t
	default:
		return theme.DefaultTheme()
	}
}

// themePackHasVariants reports whether the pack responds to the Light/Dark
// choice. The Catppuccin flavors are fixed palettes, so the variant radio is
// disabled while one is selected.
func themePackHasVariants(pack string) bool {
	return pack == themePackDefault || pack == themePackAdwaita
}

// appTheme wraps a base Fyne theme (the selected theme pack), forcing the
// configured Light/Dark variant and optionally overriding the font.
type appTheme struct {
	base     fyne.Theme
	font     fyne.Resource
	fontName string
	isDark   bool
	pack     string
}

func (t *appTheme) Color(name fyne.ThemeColorName, variant fyne.ThemeVariant) color.Color {
	// Force the configured variant rather than following the OS: the Theme
	// radio in Preferences is the single source of truth. Packs with a fixed
	// palette (Catppuccin flavors) ignore the variant entirely.
	v := fyne.ThemeVariant(theme.VariantLight)
	if t.isDark {
		v = theme.VariantDark
	}
	return t.base.Color(name, v)
}

func (t *appTheme) Font(style fyne.TextStyle) fyne.Resource {
	// Always delegate monospace requests to the base theme so that
	// TextStyle{Monospace: true} renders with a proper fixed-width font
	// regardless of which proportional font the user has selected.
	if t.font != nil && !style.Monospace {
		return t.font
	}
	return t.base.Font(style)
}

func (t *appTheme) Icon(name fyne.ThemeIconName) fyne.Resource {
	return t.base.Icon(name)
}

func (t *appTheme) Size(name fyne.ThemeSizeName) float32 {
	return t.base.Size(name)
}

func newAppTheme(isDark bool, pack string) *appTheme {
	return &appTheme{isDark: isDark, pack: pack, base: themePackBase(pack)}
}

func systemFontDirs() []string {
	switch runtime.GOOS {
	case "windows":
		windir := os.Getenv("WINDIR")
		if windir == "" {
			windir = `C:\Windows`
		}
		return []string{filepath.Join(windir, "Fonts")}
	case "darwin":
		return []string{"/Library/Fonts", "/System/Library/Fonts"}
	default:
		return []string{"/usr/share/fonts", "/usr/local/share/fonts"}
	}
}

func listSystemFonts() []string {
	seen := map[string]bool{}
	var names []string
	for _, dir := range systemFontDirs() {
		filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			ext := strings.ToLower(filepath.Ext(d.Name()))
			if ext != ".ttf" && ext != ".otf" {
				return nil
			}
			name := strings.TrimSuffix(d.Name(), filepath.Ext(d.Name()))
			if !seen[strings.ToLower(name)] {
				seen[strings.ToLower(name)] = true
				names = append(names, name)
			}
			return nil
		})
	}
	sort.Strings(names)
	return names
}

func fontPathByName(name string) string {
	for _, dir := range systemFontDirs() {
		for _, ext := range []string{".ttf", ".otf", ".TTF", ".OTF"} {
			p := filepath.Join(dir, name+ext)
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return ""
}

func loadFontResource(path string) (fyne.Resource, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return fyne.NewStaticResource(filepath.Base(path), data), nil
}

func colorToHex(c color.Color) string {
	r, g, b, a := c.RGBA()
	return fmt.Sprintf("%02X%02X%02X%02X", uint8(r>>8), uint8(g>>8), uint8(b>>8), uint8(a>>8))
}

func hexToColor(s string) color.Color {
	if len(s) == 8 {
		if v, err := strconv.ParseUint(s, 16, 32); err == nil {
			return color.RGBA{R: uint8(v >> 24), G: uint8(v >> 16), B: uint8(v >> 8), A: uint8(v)}
		}
	}
	return color.RGBA{R: 0x00, G: 0x78, B: 0xD4, A: 0xFF}
}

// boldLabel returns a label rendered in bold — the standard section-header style.
func boldLabel(text string) *widget.Label {
	l := widget.NewLabel(text)
	l.TextStyle = fyne.TextStyle{Bold: true}
	return l
}

// prefSection stacks a bold header, a separator, and the section content —
// the visual building block of the Preferences tabs.
func prefSection(title string, content ...fyne.CanvasObject) *fyne.Container {
	objs := append([]fyne.CanvasObject{boldLabel(title), widget.NewSeparator()}, content...)
	return container.NewVBox(objs...)
}

// showPreferencesDialog opens the tabbed preferences dialog: SCP & Network
// (local SCP identity, download folder, stall timeout, server profiles), User
// Interface (appearance, external viewer, tag highlights and profiles), and
// Modification & Export (de-identification profiles and defaults).
func showPreferencesDialog(a fyne.App, parent fyne.Window, current *appTheme, cfg *Settings, onApply func(Settings)) {
	if raiseOwnedWindow("preferences") {
		return
	}
	// w is the Preferences window itself, assigned as it opens at the foot of
	// this function. Everything built below — colour pickers, the server and
	// tag profile editors, confirmations, the modification-profile editor —
	// parents to w rather than to the window that opened Preferences: a child
	// parented to the latter would surface behind the blocked Preferences
	// window with no way to reach it. All such uses sit inside callbacks, so
	// the late assignment is in place long before any of them can run.
	var w fyne.Window
	themeLabel := "Light"
	if current.isDark {
		themeLabel = "Dark"
	}
	themeSelect := widget.NewRadioGroup([]string{"Light", "Dark"}, nil)
	themeSelect.SetSelected(themeLabel)

	// Colour theme pack selector. The Light/Dark radio applies only to packs
	// with variants; the fixed Catppuccin flavors disable it.
	packLabels := make([]string, len(themePackLabels))
	packByLabel := make(map[string]string, len(themePackLabels))
	currentPackLabel := themePackLabels[0].label
	for i, p := range themePackLabels {
		packLabels[i] = p.label
		packByLabel[p.label] = p.pack
		if p.pack == current.pack {
			currentPackLabel = p.label
		}
	}
	themePackSelect := widget.NewSelect(packLabels, func(label string) {
		if themePackHasVariants(packByLabel[label]) {
			themeSelect.Enable()
		} else {
			themeSelect.Disable()
		}
	})
	themePackSelect.SetSelected(currentPackLabel)

	fontSelect := widget.NewSelect([]string{"(default)", "(loading…)"}, nil)
	if current.fontName != "" {
		fontSelect.SetSelected(current.fontName)
	} else {
		fontSelect.SetSelected("(default)")
	}
	go func() {
		fonts := listSystemFonts()
		fyne.Do(func() {
			fontSelect.Options = append([]string{"(default)"}, fonts...)
			fontSelect.Refresh()
		})
	}()

	// Selected-row appearance controls (Phase 5-2E).
	chosenSelColor := hexToColor(cfg.SelectionColor) // empty/invalid → default blue
	selColorSwatch := canvas.NewRectangle(chosenSelColor)
	selColorSwatch.SetMinSize(fyne.NewSize(40, 20))
	selColorBtn := widget.NewButton("Choose colour…", func() {
		picker := dialog.NewColorPicker("Selection colour",
			"Colour applied to selected tree rows", func(c color.Color) {
				chosenSelColor = c
				selColorSwatch.FillColor = c
				selColorSwatch.Refresh()
			}, w)
		picker.Advanced = true
		picker.Show()
	})
	selBoldCheck := widget.NewCheck("Bold", nil)
	selBoldCheck.SetChecked(cfg.SelectionBold)
	selItalicCheck := widget.NewCheck("Italic", nil)
	selItalicCheck.SetChecked(cfg.SelectionItalic)

	appearanceSection := prefSection("Appearance",
		widget.NewForm(
			widget.NewFormItem("Colour theme", themePackSelect),
			widget.NewFormItem("Theme", themeSelect),
			widget.NewFormItem("Tree font", fontSelect),
			widget.NewFormItem("Selection colour", container.NewHBox(selColorSwatch, selColorBtn)),
			widget.NewFormItem("Selection style", container.NewHBox(selBoldCheck, selItalicCheck)),
		),
	)

	// Server profiles list
	pendingProfiles := append([]ServerProfile(nil), cfg.Profiles...)
	profileList := container.NewVBox()

	var buildProfileList func()
	buildProfileList = func() {
		rows := make([]fyne.CanvasObject, len(pendingProfiles))
		for i := range pendingProfiles {
			i := i
			nameLabel := widget.NewLabel(fmt.Sprintf("%s  (%s@%s:%d)", pendingProfiles[i].Name, pendingProfiles[i].RemoteAETitle, pendingProfiles[i].Host, pendingProfiles[i].Port))
			editBtn := widget.NewButton("Edit", func() {
				showServerProfileEditor(w, pendingProfiles[i], func(updated ServerProfile) {
					pendingProfiles[i] = updated
					buildProfileList()
				})
			})
			deleteBtn := widget.NewButton("Delete", func() {
				pendingProfiles = append(pendingProfiles[:i], pendingProfiles[i+1:]...)
				buildProfileList()
			})
			// Up/Down reordering buttons (Phase 4-C).
			upBtn := widget.NewButtonWithIcon("", theme.MoveUpIcon(), func() {
				if i > 0 {
					pendingProfiles[i-1], pendingProfiles[i] = pendingProfiles[i], pendingProfiles[i-1]
					buildProfileList()
				}
			})
			downBtn := widget.NewButtonWithIcon("", theme.MoveDownIcon(), func() {
				if i < len(pendingProfiles)-1 {
					pendingProfiles[i], pendingProfiles[i+1] = pendingProfiles[i+1], pendingProfiles[i]
					buildProfileList()
				}
			})
			if i == 0 {
				upBtn.Disable()
			}
			if i == len(pendingProfiles)-1 {
				downBtn.Disable()
			}
			// nameLabel as center so it expands to fill available width; buttons pin right.
			rows[i] = container.NewBorder(nil, nil, nil,
				container.NewHBox(upBtn, downBtn, editBtn, deleteBtn), nameLabel)
		}
		profileList.Objects = rows
		profileList.Refresh()
	}
	buildProfileList()

	addProfileBtn := widget.NewButton("Add server…", func() {
		showServerProfileEditor(w, ServerProfile{Name: "New Server", Port: 104, InfoModel: "study"}, func(added ServerProfile) {
			pendingProfiles = append(pendingProfiles, added)
			buildProfileList()
		})
	})

	profileScroll := container.NewVScroll(profileList)
	profileScroll.SetMinSize(fyne.NewSize(0, 160))

	serverSection := prefSection("Server Profiles", profileScroll, addProfileBtn)

	// Network — local SCP identity, download folder, stall watchdog
	localAEEntry := widget.NewEntry()
	localAEEntry.SetText(cfg.LocalAETitle)
	localPortEntry := widget.NewEntry()
	localPortEntry.SetText(fmt.Sprintf("%d", cfg.LocalSCPPort))

	downloadDirEntry := widget.NewEntry()
	downloadDirEntry.SetText(cfg.DownloadDir)
	downloadDirEntry.SetPlaceHolder("Select download folder…")
	dirBrowseBtn := widget.NewButton("Browse…", func() {
		go func() {
			dir, err := sqweekdialog.Directory().Browse()
			if err != nil {
				return
			}
			fyne.Do(func() { downloadDirEntry.SetText(dir) })
		}()
	})

	stallEntry := widget.NewEntry()
	if cfg.RetrieveStallTimeoutSec != 0 {
		stallEntry.SetText(strconv.Itoa(cfg.RetrieveStallTimeoutSec))
	}
	stallEntry.SetPlaceHolder("120 (default)")
	stallItem := widget.NewFormItem("Retrieve stall timeout (s)", stallEntry)
	stallItem.HintText = "Abort a retrieve after this many seconds without data; negative disables"

	networkSection := prefSection("Network",
		widget.NewForm(
			widget.NewFormItem("Local AE Title", localAEEntry),
			widget.NewFormItem("Local SCP port", localPortEntry),
			widget.NewFormItem("Download folder",
				container.NewBorder(nil, nil, nil, dirBrowseBtn, downloadDirEntry)),
			stallItem,
		),
	)

	// Viewer section
	viewerPathEntry := widget.NewEntry()
	viewerPathEntry.SetText(cfg.ViewerPath)
	viewerPathEntry.SetPlaceHolder("Path to DICOM viewer executable…")
	viewerBrowseBtn := widget.NewButton("Browse…", func() {
		go func() {
			path, err := sqweekdialog.File().Filter("Executable", "exe").Load()
			if err != nil {
				return
			}
			fyne.Do(func() { viewerPathEntry.SetText(path) })
		}()
	})
	detectBtn := widget.NewButton("Auto-detect", func() {
		if p := DetectDefaultViewer(); p != "" {
			viewerPathEntry.SetText(p)
		} else {
			dialog.ShowInformation("Not found",
				"No known DICOM viewer was detected.\nInstall MicroDicom or RadiAnt, or browse to your viewer manually.", w)
		}
	})

	viewerSection := prefSection("Image Viewer",
		container.NewBorder(nil, nil,
			widget.NewLabel("External viewer"),
			container.NewHBox(viewerBrowseBtn, detectBtn),
			viewerPathEntry,
		),
	)

	// Tag Highlights section — private-tag italics and the malformed-VR colour
	// used by the View Tags window (ported from dicomhdr).
	italicCheck := widget.NewCheck("Italicize", nil)
	italicCheck.SetChecked(cfg.ItalicPrivate)

	chosenMalColor := malformedTagColor(cfg)
	malSwatch := canvas.NewRectangle(chosenMalColor)
	malSwatch.SetMinSize(fyne.NewSize(40, 20))
	malColorBtn := widget.NewButton("Choose colour…", func() {
		picker := dialog.NewColorPicker("Malformed tag colour",
			"Colour applied to tags whose VR violates the standard", func(c color.Color) {
				if c == nil {
					return
				}
				r, g, b, a := c.RGBA()
				chosenMalColor = color.RGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: uint8(a >> 8)}
				malSwatch.FillColor = chosenMalColor
				malSwatch.Refresh()
			}, w)
		picker.Advanced = true
		picker.Show()
	})

	hlSection := prefSection("Tag Highlights",
		widget.NewForm(
			widget.NewFormItem("Private tags", italicCheck),
			widget.NewFormItem("Malformed tag", container.NewHBox(malSwatch, malColorBtn)),
		),
	)

	// Tag Profiles section — named tag sets coloured in the View Tags window.
	pendingTagProfiles := append([]TagProfile(nil), cfg.TagProfiles...)
	tagProfileList := container.NewVBox()

	var buildTagProfileList func()
	buildTagProfileList = func() {
		rows := make([]fyne.CanvasObject, len(pendingTagProfiles))
		for i := range pendingTagProfiles {
			i := i
			check := widget.NewCheck("", func(enabled bool) {
				pendingTagProfiles[i].Enabled = enabled
			})
			check.SetChecked(pendingTagProfiles[i].Enabled)
			nameLabel := widget.NewLabel(fmt.Sprintf("%s  (%d tags)",
				pendingTagProfiles[i].Name, len(pendingTagProfiles[i].Tags)))
			editBtn := widget.NewButton("Edit", func() {
				showTagProfileEditor(w, pendingTagProfiles[i], func(updated TagProfile) {
					updated.Enabled = pendingTagProfiles[i].Enabled
					pendingTagProfiles[i] = updated
					buildTagProfileList()
				})
			})
			deleteBtn := widget.NewButton("Delete", func() {
				pendingTagProfiles = append(pendingTagProfiles[:i], pendingTagProfiles[i+1:]...)
				buildTagProfileList()
			})
			rows[i] = container.NewBorder(nil, nil,
				container.NewHBox(check, nameLabel),
				container.NewHBox(editBtn, deleteBtn),
			)
		}
		tagProfileList.Objects = rows
		tagProfileList.Refresh()
	}
	buildTagProfileList()

	addTagProfileBtn := widget.NewButton("Add profile…", func() {
		newP := TagProfile{
			Name:    "New Profile",
			Color:   color.RGBA{R: 0x00, G: 0x80, B: 0xFF, A: 0xFF},
			Enabled: true,
		}
		showTagProfileEditor(w, newP, func(added TagProfile) {
			pendingTagProfiles = append(pendingTagProfiles, added)
			buildTagProfileList()
		})
	})

	tagProfileScroll := container.NewVScroll(tagProfileList)
	tagProfileScroll.SetMinSize(fyne.NewSize(0, 120))

	tpSection := prefSection("Tag Profiles", tagProfileScroll, addTagProfileBtn)

	// Modification profiles — the de-identification recipes in
	// ~/.dicomqr/profiles.json. Edited as a working copy like the other lists
	// and committed only on Apply, and only when something actually changed —
	// the file is hand-editable and must never be rewritten gratuitously. If it
	// fails to parse, editing is disabled and Apply never overwrites it.
	var (
		loadedModProfiles  ModProfileConfig
		modProfilesLoadErr error
	)
	if profPath, perr := modifyProfilesPath(); perr != nil {
		modProfilesLoadErr = perr
	} else {
		loadedModProfiles, modProfilesLoadErr = loadModProfileConfig(profPath)
	}
	pendingModProfiles := maps.Clone(loadedModProfiles)
	if pendingModProfiles == nil {
		pendingModProfiles = ModProfileConfig{}
	}

	modProfileList := container.NewVBox()
	var buildModProfileList func()
	buildModProfileList = func() {
		names := make([]string, 0, len(pendingModProfiles))
		for n := range pendingModProfiles {
			names = append(names, n)
		}
		sort.Strings(names)
		rows := make([]fyne.CanvasObject, len(names))
		for i, n := range names {
			n := n
			p := pendingModProfiles[n]
			desc := fmt.Sprintf("%s  (%d set, %d remove", n, len(p.Sets), len(p.Removes))
			if p.Base != "" {
				desc += ", base: " + p.Base
			}
			if len(p.PerModality) > 0 {
				desc += fmt.Sprintf(", %d per-modality", len(p.PerModality))
			}
			desc += ")"
			nameLabel := widget.NewLabel(desc)
			editBtn := widget.NewButton("Edit", func() {
				showModProfileEditor(a, w, n, pendingModProfiles[n], pendingModProfiles, cfg.DownloadDir,
					func(newName string, updated ModProfile) {
						if newName != n {
							delete(pendingModProfiles, n)
							// Keep base chains intact across a rename.
							for on, op := range pendingModProfiles {
								if op.Base == n {
									op.Base = newName
									pendingModProfiles[on] = op
								}
							}
						}
						pendingModProfiles[newName] = updated
						buildModProfileList()
					})
			})
			deleteBtn := widget.NewButton("Delete", func() {
				var dependents []string
				for on, op := range pendingModProfiles {
					if on != n && op.Base == n {
						dependents = append(dependents, on)
					}
				}
				doDelete := func() {
					delete(pendingModProfiles, n)
					buildModProfileList()
				}
				if len(dependents) > 0 {
					sort.Strings(dependents)
					dialog.ShowConfirm("Delete profile",
						fmt.Sprintf("%q is the base of: %s.\nDeleting it will break those profiles. Delete anyway?",
							n, strings.Join(dependents, ", ")),
						func(ok bool) {
							if ok {
								doDelete()
							}
						}, w)
					return
				}
				doDelete()
			})
			rows[i] = container.NewBorder(nil, nil, nil,
				container.NewHBox(editBtn, deleteBtn), nameLabel)
		}
		modProfileList.Objects = rows
		modProfileList.Refresh()
	}

	addModProfileBtn := widget.NewButton("Add profile…", func() {
		showModProfileEditor(a, w, "", ModProfile{}, pendingModProfiles, cfg.DownloadDir,
			func(newName string, added ModProfile) {
				pendingModProfiles[newName] = added
				buildModProfileList()
			})
	})

	var modProfileSection fyne.CanvasObject
	if modProfilesLoadErr != nil {
		errLbl := widget.NewLabel(fmt.Sprintf(
			"profiles.json could not be read: %v\n\nFix or delete the file to enable profile editing. Apply will not overwrite it.",
			modProfilesLoadErr))
		errLbl.Wrapping = fyne.TextWrapWord
		modProfileSection = prefSection("Modification Profiles", errLbl)
	} else {
		buildModProfileList()
		modProfileScroll := container.NewVScroll(modProfileList)
		modProfileScroll.SetMinSize(fyne.NewSize(0, 140))
		modProfileSection = prefSection("Modification Profiles", modProfileScroll, addModProfileBtn)
	}

	// Modification/export defaults
	modOutDirEntry := widget.NewEntry()
	modOutDirEntry.SetText(cfg.ModifyOutputDir)
	modOutDirEntry.SetPlaceHolder("No default — the first folder chosen in the Modification dialog is saved here")
	modOutBrowseBtn := widget.NewButton("Browse…", func() {
		go func() {
			dir, err := sqweekdialog.Directory().Title("Choose default output folder for modified files").Browse()
			if err != nil {
				return
			}
			fyne.Do(func() { modOutDirEntry.SetText(dir) })
		}()
	})
	modOutItem := widget.NewFormItem("Default output folder",
		container.NewBorder(nil, nil, nil, modOutBrowseBtn, modOutDirEntry))
	modOutItem.HintText = "Used directly by the Modification dialog (no picker on routine runs); must be outside the download folder"

	exportFormatSelect := widget.NewSelect([]string{"CSV", "JSON"}, nil)
	if strings.EqualFold(cfg.ExportFormat, "json") {
		exportFormatSelect.SetSelected("JSON")
	} else {
		exportFormatSelect.SetSelected("CSV")
	}

	exportFmtItem := widget.NewFormItem("Default export format", exportFormatSelect)
	exportFmtItem.HintText = "File type listed first in the View Tags Export Tags… save dialog"

	defaultsSection := prefSection("Defaults",
		widget.NewForm(
			modOutItem,
			exportFmtItem,
		),
	)

	cancelBtn := widget.NewButton("Cancel", func() { w.Close() })
	applyBtn := widget.NewButton("Apply", func() {
		current.isDark = themeSelect.Selected == "Dark"
		current.pack = packByLabel[themePackSelect.Selected]
		current.base = themePackBase(current.pack)
		if fontSelect.Selected == "(default)" {
			current.font = nil
			current.fontName = ""
		} else {
			if path := fontPathByName(fontSelect.Selected); path != "" {
				if res, err := loadFontResource(path); err == nil {
					current.font = res
					current.fontName = fontSelect.Selected
				}
			}
		}

		port := cfg.LocalSCPPort
		if p, err := strconv.Atoi(localPortEntry.Text); err == nil && p > 0 && p < 65536 {
			port = p
		}
		stall := cfg.RetrieveStallTimeoutSec
		if s := strings.TrimSpace(stallEntry.Text); s == "" {
			stall = 0 // blank = use the built-in default (120 s)
		} else if v, err := strconv.Atoi(s); err == nil {
			stall = v
		}

		// Copy-then-overwrite: fields without dialog controls (window size,
		// anything added later) carry through instead of being silently zeroed.
		updated := *cfg
		updated.DarkTheme = current.isDark
		updated.UITheme = current.pack
		updated.FontName = current.fontName
		updated.LocalAETitle = localAEEntry.Text
		updated.LocalSCPPort = port
		updated.DownloadDir = downloadDirEntry.Text
		updated.Profiles = pendingProfiles
		updated.SelectionColor = colorToHex(chosenSelColor)
		updated.SelectionBold = selBoldCheck.Checked
		updated.SelectionItalic = selItalicCheck.Checked
		updated.ViewerPath = viewerPathEntry.Text
		updated.ItalicPrivate = italicCheck.Checked
		updated.MalformedColor = colorToHex(chosenMalColor)
		updated.TagProfiles = pendingTagProfiles
		updated.RetrieveStallTimeoutSec = stall
		updated.ModifyOutputDir = strings.TrimSpace(modOutDirEntry.Text)
		if exportFormatSelect.Selected == "JSON" {
			updated.ExportFormat = "json"
		} else {
			updated.ExportFormat = "csv"
		}

		if updated.ModifyOutputDir != "" && pathWithinDir(updated.ModifyOutputDir, updated.DownloadDir) {
			dialog.ShowError(fmt.Errorf(
				"the default modification output folder must be outside the download folder (%s) — modified files are never mixed into the local index",
				updated.DownloadDir), w)
			return
		}

		// Commit modification-profile edits. Nothing else holds the working
		// copy, so a failed save means the edits are gone once the dialog
		// closes — say so.
		if modProfilesLoadErr == nil && !reflect.DeepEqual(pendingModProfiles, loadedModProfiles) {
			profPath, perr := modifyProfilesPath()
			if perr == nil {
				perr = saveModProfileConfig(profPath, pendingModProfiles)
			}
			if perr != nil {
				dialog.ShowError(fmt.Errorf(
					"Modification profile changes could not be saved to disk and will be lost:\n\n%v", perr), w)
			}
		}

		if err := saveSettingsE(updated); err != nil {
			// Apply for this session regardless, but make the persistence
			// failure impossible to miss — losing a mid-session preference
			// change silently is how misconfigurations go unnoticed.
			dialog.ShowError(fmt.Errorf(
				"Settings were applied for this session but could not be saved to disk:\n\n%v", err), w)
		}
		a.Settings().SetTheme(current)
		onApply(updated)
		refreshOpenTagViewers()
		w.Close()
	})

	buttonRow := container.NewBorder(
		widget.NewSeparator(), nil, nil, nil,
		container.NewPadded(container.NewBorder(nil, nil, nil, container.NewHBox(cancelBtn, applyBtn))),
	)

	// Three tabs, each scrolling independently; the button row stays pinned
	// below the tab container so Cancel/Apply are always visible.
	tabs := container.NewAppTabs(
		container.NewTabItem("SCP & Network",
			container.NewVScroll(container.NewVBox(networkSection, serverSection))),
		container.NewTabItem("User Interface",
			container.NewVScroll(container.NewVBox(appearanceSection, viewerSection, hlSection, tpSection))),
		container.NewTabItem("Modification & Export",
			container.NewVScroll(container.NewVBox(modProfileSection, defaultsSection))),
	)

	minSize := canvas.NewRectangle(color.Transparent)
	minSize.SetMinSize(fyne.NewSize(640, 520))
	content := container.NewStack(minSize,
		container.NewBorder(nil, buttonRow, nil, nil, tabs))
	// Preferences owns the modification-profile editors it opens, so closing
	// it takes them with it; blocking the window behind it preserves what the
	// dialog gave for free, and matters because Apply writes a whole settings
	// snapshot back — letting the main window be changed underneath would
	// silently discard those changes.
	openOwnedWindow(a, windowSpec{
		Key:      "preferences",
		Title:    "Preferences",
		Size:     fyne.NewSize(720, 640),
		Parent:   parent,
		Blocking: true,
	}, func(win fyne.Window) fyne.CanvasObject {
		w = win
		return content
	})
}

// showServerProfileEditor opens an edit dialog for a single ServerProfile.
func showServerProfileEditor(w fyne.Window, p ServerProfile, onSave func(ServerProfile)) {
	nameEntry := widget.NewEntry()
	nameEntry.SetText(p.Name)

	aeEntry := widget.NewEntry()
	aeEntry.SetText(p.RemoteAETitle)
	aeEntry.Validator = func(s string) error {
		s = strings.TrimSpace(s)
		if s == "" {
			return fmt.Errorf("AE title is required")
		}
		if len(s) > 16 {
			return fmt.Errorf("AE title must be ≤ 16 characters")
		}
		return nil
	}

	hostEntry := widget.NewEntry()
	hostEntry.SetText(p.Host)

	portEntry := widget.NewEntry()
	portEntry.SetText(fmt.Sprintf("%d", p.Port))
	portEntry.Validator = func(s string) error {
		v, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || v < 1 || v > 65535 {
			return fmt.Errorf("port must be 1–65535")
		}
		return nil
	}

	timeoutEntry := widget.NewEntry()
	if p.ConnectTimeout > 0 {
		timeoutEntry.SetText(fmt.Sprintf("%d", p.ConnectTimeout))
	}
	timeoutEntry.SetPlaceHolder("10 (default)")

	modelSelect := widget.NewSelect([]string{"study", "patient", "patient-study-only"}, nil)
	modelSelect.SetSelected(p.InfoModel)

	retrieveMethodSelect := widget.NewSelect([]string{"C-MOVE (default)", "C-GET", "Auto"}, nil)
	switch p.RetrieveMethod {
	case "GET":
		retrieveMethodSelect.SetSelected("C-GET")
	case "AUTO":
		retrieveMethodSelect.SetSelected("Auto")
	default:
		retrieveMethodSelect.SetSelected("C-MOVE (default)")
	}

	// Requiring a syntax guarantees every retrieved file is in it on disk:
	// the server sends it directly, or the file is converted on receipt from
	// a decodable syntax; otherwise the retrieve fails with an error.
	const (
		tsLabelAny      = "As stored (server decides)"
		tsLabelExplicit = "Explicit VR LE (uncompressed — convert locally if needed)"
		tsLabelImplicit = "Implicit VR LE (uncompressed — convert locally if needed)"
	)
	tsSelect := widget.NewSelect([]string{tsLabelAny, tsLabelExplicit, tsLabelImplicit}, nil)
	switch p.TransferSyntax {
	case tsPrefExplicitLE:
		tsSelect.SetSelected(tsLabelExplicit)
	case tsPrefImplicitLE:
		tsSelect.SetSelected(tsLabelImplicit)
	default:
		tsSelect.SetSelected(tsLabelAny)
	}

	form := widget.NewForm(
		widget.NewFormItem("Profile name", nameEntry),
		widget.NewFormItem("Remote AE Title", aeEntry),
		widget.NewFormItem("Host", hostEntry),
		widget.NewFormItem("Port", portEntry),
		widget.NewFormItem("Connect timeout (s)", timeoutEntry),
		widget.NewFormItem("Info model", modelSelect),
		widget.NewFormItem("Retrieve method", retrieveMethodSelect),
		widget.NewFormItem("Transfer syntax", tsSelect),
	)

	d := dialog.NewCustomConfirm("Edit Server", "Save", "Cancel", form, func(save bool) {
		if !save {
			return
		}
		// Inline validation (Phase 3-I): show error and abort if invalid.
		if err := aeEntry.Validate(); err != nil {
			dialog.ShowError(err, w)
			return
		}
		if err := portEntry.Validate(); err != nil {
			dialog.ShowError(err, w)
			return
		}
		port := p.Port
		if v, err := strconv.Atoi(strings.TrimSpace(portEntry.Text)); err == nil && v > 0 && v < 65536 {
			port = v
		}
		timeout := 0
		if v, err := strconv.Atoi(strings.TrimSpace(timeoutEntry.Text)); err == nil && v > 0 {
			timeout = v
		}
		retrieveMethod := "MOVE"
		switch retrieveMethodSelect.Selected {
		case "C-GET":
			retrieveMethod = "GET"
		case "Auto":
			retrieveMethod = "AUTO"
		}
		transferSyntax := tsPrefAny
		switch tsSelect.Selected {
		case tsLabelExplicit:
			transferSyntax = tsPrefExplicitLE
		case tsLabelImplicit:
			transferSyntax = tsPrefImplicitLE
		}
		onSave(ServerProfile{
			Name:           nameEntry.Text,
			RemoteAETitle:  strings.ToUpper(strings.TrimSpace(aeEntry.Text)),
			Host:           strings.TrimSpace(hostEntry.Text),
			Port:           port,
			ConnectTimeout: timeout,
			InfoModel:      modelSelect.Selected,
			RetrieveMethod: retrieveMethod,
			TransferSyntax: transferSyntax,
		})
	}, w)
	// Widen beyond the form's natural minimum so the transfer syntax options
	// ("Explicit VR LE (uncompressed — convert locally if needed)") are fully
	// readable in the select and its dropdown.
	sz := d.MinSize()
	if sz.Width < 640 {
		sz.Width = 640
	}
	d.Resize(sz)
	d.Show()
}
