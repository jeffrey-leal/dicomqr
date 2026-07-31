package main

// Adwaita colour scheme for Fyne, vendored from the fyne-x community
// extensions repository (fyne.io/x/fyne/theme, BSD-3-Clause, © the Fyne.io
// authors) rather than imported: the fyne-x module tracks Fyne's master
// branch, and depending on it silently upgrades fyne itself (2.7.3 → 2.8.0
// plus a pre-release glfw at the time of writing) — an unacceptable side
// effect for a theme palette. Only the colour tables are vendored; fyne-x's
// Adwaita icon set is omitted, so icons stay Fyne's stock set.
//
// The colours are generated from the libadwaita named-colour specification:
// https://gnome.pages.gitlab.gnome.org/libadwaita/doc/main/named-colors.html

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
)

var _ fyne.Theme = (*adwaitaTheme)(nil)

// adwaitaTheme recolours the app to the GNOME Adwaita specification, with
// light and dark variants. Fonts, icons, and sizes delegate to the default
// theme.
type adwaitaTheme struct{}

func (a adwaitaTheme) Color(name fyne.ThemeColorName, variant fyne.ThemeVariant) color.Color {
	switch variant {
	case theme.VariantLight:
		if c, ok := adwaitaLightScheme[name]; ok {
			return c
		}
	case theme.VariantDark:
		if c, ok := adwaitaDarkScheme[name]; ok {
			return c
		}
	}
	return theme.DefaultTheme().Color(name, variant)
}

func (a adwaitaTheme) Font(style fyne.TextStyle) fyne.Resource {
	return theme.DefaultTheme().Font(style)
}

func (a adwaitaTheme) Icon(name fyne.ThemeIconName) fyne.Resource {
	return theme.DefaultTheme().Icon(name)
}

func (a adwaitaTheme) Size(name fyne.ThemeSizeName) float32 {
	return theme.DefaultTheme().Size(name)
}

var adwaitaDarkScheme = map[fyne.ThemeColorName]color.Color{
	theme.ColorBlue:                  color.NRGBA{R: 0x35, G: 0x84, B: 0xe4, A: 0xff}, // @blue_3
	theme.ColorBrown:                 color.NRGBA{R: 0x98, G: 0x6a, B: 0x44, A: 0xff}, // @brown_3
	theme.ColorGray:                  color.NRGBA{R: 0x5e, G: 0x5c, B: 0x64, A: 0xff}, // @dark_2
	theme.ColorGreen:                 color.NRGBA{R: 0x26, G: 0xa2, B: 0x69, A: 0xff}, // @green_5
	theme.ColorNameBackground:        color.NRGBA{R: 0x24, G: 0x24, B: 0x24, A: 0xff}, // @window_bg_color
	theme.ColorNameButton:            color.NRGBA{R: 0x30, G: 0x30, B: 0x30, A: 0xff}, // @headerbar_bg_color
	theme.ColorNameError:             color.NRGBA{R: 0xc0, G: 0x1c, B: 0x28, A: 0xff}, // @error_bg_color
	theme.ColorNameForeground:        color.NRGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}, // @window_fg_color
	theme.ColorNameInputBackground:   color.NRGBA{R: 0x1e, G: 0x1e, B: 0x1e, A: 0xff}, // @view_bg_color
	theme.ColorNameMenuBackground:    color.NRGBA{R: 0x38, G: 0x38, B: 0x38, A: 0xff}, // @popover_bg_color
	theme.ColorNameOverlayBackground: color.NRGBA{R: 0x1e, G: 0x1e, B: 0x1e, A: 0xff}, // @view_bg_color
	theme.ColorNamePrimary:           color.NRGBA{R: 0x35, G: 0x84, B: 0xe4, A: 0xff}, // @accent_bg_color
	theme.ColorNameScrollBar:         color.NRGBA{R: 0xff, G: 0xff, B: 0xff, A: 0x5b}, // @light_1
	theme.ColorNameSelection:         color.NRGBA{R: 0x30, G: 0x30, B: 0x30, A: 0xff}, // @headerbar_bg_color
	theme.ColorNameShadow:            color.NRGBA{R: 0x00, G: 0x00, B: 0x00, A: 0x5b}, // @shade_color
	theme.ColorNameSuccess:           color.NRGBA{R: 0x26, G: 0xa2, B: 0x69, A: 0xff}, // @success_bg_color
	theme.ColorNameWarning:           color.NRGBA{R: 0xcd, G: 0x93, B: 0x09, A: 0xff}, // @warning_bg_color
	theme.ColorOrange:                color.NRGBA{R: 0xff, G: 0x78, B: 0x00, A: 0xff}, // @orange_3
	theme.ColorPurple:                color.NRGBA{R: 0x91, G: 0x41, B: 0xac, A: 0xff}, // @purple_3
	theme.ColorRed:                   color.NRGBA{R: 0xc0, G: 0x1c, B: 0x28, A: 0xff}, // @red_4
	theme.ColorYellow:                color.NRGBA{R: 0xf6, G: 0xd3, B: 0x2d, A: 0xff}, // @yellow_3
}

var adwaitaLightScheme = map[fyne.ThemeColorName]color.Color{
	theme.ColorBlue:                  color.NRGBA{R: 0x35, G: 0x84, B: 0xe4, A: 0xff}, // @blue_3
	theme.ColorBrown:                 color.NRGBA{R: 0x98, G: 0x6A, B: 0x44, A: 0xff}, // @brown_3
	theme.ColorGray:                  color.NRGBA{R: 0x5e, G: 0x5C, B: 0x64, A: 0xff}, // @dark_2
	theme.ColorGreen:                 color.NRGBA{R: 0x2e, G: 0xC2, B: 0x7e, A: 0xff}, // @green_4
	theme.ColorNameBackground:        color.NRGBA{R: 0xfa, G: 0xFA, B: 0xfa, A: 0xff}, // @window_bg_color
	theme.ColorNameButton:            color.NRGBA{R: 0xeb, G: 0xEB, B: 0xeb, A: 0xff}, // @headerbar_bg_color
	theme.ColorNameError:             color.NRGBA{R: 0xe0, G: 0x1B, B: 0x24, A: 0xff}, // @error_bg_color
	theme.ColorNameForeground:        color.NRGBA{R: 0x00, G: 0x00, B: 0x00, A: 0xcc}, // @window_fg_color
	theme.ColorNameInputBackground:   color.NRGBA{R: 0xff, G: 0xFF, B: 0xff, A: 0xff}, // @view_bg_color
	theme.ColorNameMenuBackground:    color.NRGBA{R: 0xff, G: 0xFF, B: 0xff, A: 0xff}, // @popover_bg_color
	theme.ColorNameOverlayBackground: color.NRGBA{R: 0xff, G: 0xFF, B: 0xff, A: 0xff}, // @view_bg_color
	theme.ColorNamePrimary:           color.NRGBA{R: 0x35, G: 0x84, B: 0xe4, A: 0xff}, // @accent_bg_color
	theme.ColorNameScrollBar:         color.NRGBA{R: 0x00, G: 0x00, B: 0x00, A: 0x5b}, // @dark_5
	theme.ColorNameSelection:         color.NRGBA{R: 0xeb, G: 0xEB, B: 0xeb, A: 0xff}, // @headerbar_bg_color
	theme.ColorNameShadow:            color.NRGBA{R: 0x00, G: 0x00, B: 0x00, A: 0x11}, // @shade_color
	theme.ColorNameSuccess:           color.NRGBA{R: 0x2e, G: 0xC2, B: 0x7e, A: 0xff}, // @success_bg_color
	theme.ColorNameWarning:           color.NRGBA{R: 0xe5, G: 0xA5, B: 0x0a, A: 0xff}, // @warning_bg_color
	theme.ColorOrange:                color.NRGBA{R: 0xff, G: 0x78, B: 0x00, A: 0xff}, // @orange_3
	theme.ColorPurple:                color.NRGBA{R: 0x91, G: 0x41, B: 0xac, A: 0xff}, // @purple_3
	theme.ColorRed:                   color.NRGBA{R: 0xe0, G: 0x1B, B: 0x24, A: 0xff}, // @red_3
	theme.ColorYellow:                color.NRGBA{R: 0xf6, G: 0xD3, B: 0x2d, A: 0xff}, // @yellow_3
}
