package main

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// queryRow is the canvas object used for each row in the results tree.
// Mirrors treeRow from dicomhdr: tap to select, right-click context menu.
// Also implements desktop.Mouseable, solely to read the Ctrl/Shift modifiers
// that decide what a click does to the selection (nodeSelection.Click) —
// fyne.PointEvent (what Tapped receives) carries none. Fyne's driver calls
// MouseUp immediately before Tapped for the same click (and Tapped only ever
// fires after a primary-button release), so capturing the modifiers in
// MouseUp and consuming them in Tapped needs no state that survives across
// separate clicks.
type queryRow struct {
	widget.BaseWidget
	ct       *canvas.Text
	nodeID   string
	onTapped func(id string, mods fyne.KeyModifier)
	onMenu   func(id string, pos fyne.Position)
	mods     fyne.KeyModifier
}

func newQueryRow(onTapped func(id string, mods fyne.KeyModifier), onMenu func(id string, pos fyne.Position)) *queryRow {
	qr := &queryRow{
		ct:       canvas.NewText("", theme.Color(theme.ColorNameForeground)),
		onTapped: onTapped,
		onMenu:   onMenu,
	}
	qr.ExtendBaseWidget(qr)
	return qr
}

func (qr *queryRow) Tapped(*fyne.PointEvent) {
	if qr.onTapped != nil && qr.nodeID != "" {
		qr.onTapped(qr.nodeID, qr.mods)
	}
}

func (qr *queryRow) MouseDown(*desktop.MouseEvent) {}

func (qr *queryRow) MouseUp(e *desktop.MouseEvent) {
	qr.mods = e.Modifier
}

func (qr *queryRow) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(container.New(rowLayout{}, qr.ct))
}

func (qr *queryRow) TappedSecondary(e *fyne.PointEvent) {
	if qr.onMenu != nil && qr.nodeID != "" {
		qr.onMenu(qr.nodeID, e.AbsolutePosition)
	}
}
