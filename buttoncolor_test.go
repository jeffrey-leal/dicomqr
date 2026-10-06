package main

import (
	"testing"

	"fyne.io/fyne/v2/theme"
)

// Every theme pack, in every variant it offers, draws ordinary buttons with a
// fill that stands apart from the background — the stock light theme's
// #F5F5F5-on-white read as a bold label — while their text stays readable on
// that fill. Thresholds: a button fill at least 1.3:1 against the background
// (the stock light fill was 1.09:1), and text at least 4.5:1 against the fill
// (WCAG AA for body text).
func TestButtonsStandOutInEveryTheme(t *testing.T) {
	for _, p := range themePackLabels {
		variants := []bool{false}
		if themePackHasVariants(p.pack) {
			variants = []bool{false, true}
		}
		for _, dark := range variants {
			th := newAppTheme(dark, p.pack)
			bg := th.Color(theme.ColorNameBackground, theme.VariantLight)
			fg := th.Color(theme.ColorNameForeground, theme.VariantLight)
			btn := th.Color(theme.ColorNameButton, theme.VariantLight)
			t.Logf("%-28s dark=%-5v button %v: %.2f:1 on background, text %.2f:1", p.label, dark, btn,
				contrastRatio(btn, bg), contrastRatio(fg, btn))
			if r := contrastRatio(btn, bg); r < 1.3 {
				t.Errorf("%s dark=%v: button %v on background %v is %.2f:1, want >= 1.3", p.label, dark, btn, bg, r)
			}
			if r := contrastRatio(fg, btn); r < 4.5 {
				t.Errorf("%s dark=%v: text %v on button %v is %.2f:1, want >= 4.5", p.label, dark, fg, btn, r)
			}
		}
	}
}

// A pack whose own button fill already stands out more keeps it: the override
// only ever increases the separation, never flattens a palette's own choice.
func TestDistinctButtonColorKeepsStrongerOwnFill(t *testing.T) {
	bg := theme.DefaultTheme().Color(theme.ColorNameBackground, theme.VariantLight)
	fg := theme.DefaultTheme().Color(theme.ColorNameForeground, theme.VariantLight)
	strong := mixColor(bg, fg, 0.5)
	if got := distinctButtonColor(bg, fg, strong); got != strong {
		t.Errorf("stronger own fill replaced: got %v, want %v", got, strong)
	}
	weak := theme.DefaultTheme().Color(theme.ColorNameButton, theme.VariantLight)
	if got := distinctButtonColor(bg, fg, weak); contrastRatio(got, bg) <= contrastRatio(weak, bg) {
		t.Errorf("weak own fill %v not strengthened: got %v", weak, got)
	}
}
