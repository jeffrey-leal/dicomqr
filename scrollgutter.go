package main

// The gap that keeps nested scrollbars grabbable.
//
// Fyne draws a scroll container's vertical scrollbar as an overlay along the
// inside of its right edge — a band about theme.SizeNameScrollBar wide that
// expands under the pointer, and is dragged there. When a widget that scrolls
// itself (a widget.List, a multiline widget.Entry, a nested VScroll) sits
// inside a dialog body that also scrolls, and its right edge is closer than
// that band's width to the outer edge, the two grab zones lie on top of each
// other: aiming for the dialog's scrollbar catches the inner control's, and
// vice versa. Every scrolling dialog body whose content embeds its own
// vertically-scrolling widget routes through this file, so the clearance is
// decided once (Preferences' tabs, the Modification dialog, the profile
// editor, the per-modality sub-editor).

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
)

// scrollGutterWidth is the horizontal clearance an inner vertically-scrolling
// widget needs between its right edge and the right edge of the outer scroll
// holding it: the outer bar's full grab band, plus one padding of visible air
// between the two bars. Derived from the theme rather than hard-coded, so the
// guarantee survives a theme that sizes its scrollbars differently.
func scrollGutterWidth() float32 {
	return theme.Size(theme.SizeNameScrollBar) + theme.Size(theme.SizeNamePadding)
}

// padForScrollbar wraps obj with scrollGutterWidth of padding on its right —
// the shape a scrolling body's content takes when anything inside it scrolls
// on its own.
func padForScrollbar(obj fyne.CanvasObject) fyne.CanvasObject {
	return container.New(layout.NewCustomPaddedLayout(0, 0, 0, scrollGutterWidth()), obj)
}
