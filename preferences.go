package main

import (
	"fmt"
	"image/color"
	"io/fs"
	"os"
	"path/filepath"
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
)

// appTheme wraps a base Fyne theme and optionally overrides the font.
type appTheme struct {
	base     fyne.Theme
	font     fyne.Resource
	fontName string
	isDark   bool
}

func (t *appTheme) Color(name fyne.ThemeColorName, variant fyne.ThemeVariant) color.Color {
	return t.base.Color(name, variant)
}

func (t *appTheme) Font(style fyne.TextStyle) fyne.Resource {
	if t.font != nil {
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

func newAppTheme(isDark bool) *appTheme {
	t := &appTheme{isDark: isDark}
	if isDark {
		t.base = theme.DarkTheme()
	} else {
		t.base = theme.LightTheme()
	}
	return t
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

// showPreferencesDialog opens the preferences dialog with UI, Connections, and Retrieve sections.
func showPreferencesDialog(a fyne.App, w fyne.Window, current *appTheme, cfg *Settings, onApply func(Settings)) {
	themeLabel := "Light"
	if current.isDark {
		themeLabel = "Dark"
	}
	themeSelect := widget.NewRadioGroup([]string{"Light", "Dark"}, nil)
	themeSelect.SetSelected(themeLabel)

	fontOptions := append([]string{"(default)"}, listSystemFonts()...)
	fontSelect := widget.NewSelect(fontOptions, nil)
	if current.fontName != "" {
		fontSelect.SetSelected(current.fontName)
	} else {
		fontSelect.SetSelected("(default)")
	}

	// UI section
	uiHeader := widget.NewLabel("UI")
	uiHeader.TextStyle = fyne.TextStyle{Bold: true}
	uiSection := container.NewVBox(
		uiHeader,
		widget.NewSeparator(),
		widget.NewForm(
			widget.NewFormItem("Theme", themeSelect),
			widget.NewFormItem("Tree font", fontSelect),
		),
	)

	// Connections section — server profiles
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
			// nameLabel as center so it expands to fill available width; buttons pin right.
			rows[i] = container.NewBorder(nil, nil, nil, container.NewHBox(editBtn, deleteBtn), nameLabel)
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

	connHeader := widget.NewLabel("Connections")
	connHeader.TextStyle = fyne.TextStyle{Bold: true}
	connSection := container.NewVBox(
		connHeader,
		widget.NewSeparator(),
		profileScroll,
		addProfileBtn,
	)

	// Retrieve section
	localAEEntry := widget.NewEntry()
	localAEEntry.SetText(cfg.LocalAETitle)
	localPortEntry := widget.NewEntry()
	localPortEntry.SetText(fmt.Sprintf("%d", cfg.LocalSCPPort))

	retHeader := widget.NewLabel("Retrieve")
	retHeader.TextStyle = fyne.TextStyle{Bold: true}
	retSection := container.NewVBox(
		retHeader,
		widget.NewSeparator(),
		widget.NewForm(
			widget.NewFormItem("Local AE Title", localAEEntry),
			widget.NewFormItem("Local SCP port", localPortEntry),
		),
	)

	var d dialog.Dialog
	cancelBtn := widget.NewButton("Cancel", func() { d.Hide() })
	applyBtn := widget.NewButton("Apply", func() {
		current.isDark = themeSelect.Selected == "Dark"
		if current.isDark {
			current.base = theme.DarkTheme()
		} else {
			current.base = theme.LightTheme()
		}
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

		updated := Settings{
			DarkTheme:    current.isDark,
			FontName:     current.fontName,
			LocalAETitle: localAEEntry.Text,
			LocalSCPPort: port,
			DownloadDir:  cfg.DownloadDir,
			Profiles:     pendingProfiles,
		}
		saveSettings(updated)
		a.Settings().SetTheme(current)
		onApply(updated)
		d.Hide()
	})

	buttonRow := container.NewBorder(
		widget.NewSeparator(), nil, nil, nil,
		container.NewPadded(container.NewBorder(nil, nil, nil, container.NewHBox(cancelBtn, applyBtn))),
	)

	minWidth := canvas.NewRectangle(color.Transparent)
	minWidth.SetMinSize(fyne.NewSize(640, 0))
	content := container.NewStack(minWidth, container.NewVBox(uiSection, connSection, retSection, buttonRow))
	d = dialog.NewCustomWithoutButtons("Preferences", content, w)
	d.Show()
}

// showServerProfileEditor opens an edit dialog for a single ServerProfile.
func showServerProfileEditor(w fyne.Window, p ServerProfile, onSave func(ServerProfile)) {
	nameEntry := widget.NewEntry()
	nameEntry.SetText(p.Name)
	aeEntry := widget.NewEntry()
	aeEntry.SetText(p.RemoteAETitle)
	hostEntry := widget.NewEntry()
	hostEntry.SetText(p.Host)
	portEntry := widget.NewEntry()
	portEntry.SetText(fmt.Sprintf("%d", p.Port))
	modelSelect := widget.NewSelect([]string{"study", "patient"}, nil)
	modelSelect.SetSelected(p.InfoModel)

	form := widget.NewForm(
		widget.NewFormItem("Profile name", nameEntry),
		widget.NewFormItem("Remote AE Title", aeEntry),
		widget.NewFormItem("Host", hostEntry),
		widget.NewFormItem("Port", portEntry),
		widget.NewFormItem("Info model", modelSelect),
	)

	dialog.ShowCustomConfirm("Edit Server", "Save", "Cancel", form, func(save bool) {
		if !save {
			return
		}
		port := p.Port
		if v, err := strconv.Atoi(portEntry.Text); err == nil && v > 0 && v < 65536 {
			port = v
		}
		onSave(ServerProfile{
			Name:          nameEntry.Text,
			RemoteAETitle: strings.ToUpper(strings.TrimSpace(aeEntry.Text)),
			Host:          strings.TrimSpace(hostEntry.Text),
			Port:          port,
			InfoModel:     modelSelect.Selected,
		})
	}, w)
}
