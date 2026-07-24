package main

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// queryRow is the canvas object used for each row in the results tree.
// Mirrors treeRow from dicomhdr: tap to select, right-click context menu.
type queryRow struct {
	widget.BaseWidget
	ct       *canvas.Text
	nodeID   string
	onTapped func(id string)
	onMenu   func(id string, pos fyne.Position)
}

func newQueryRow(onTapped func(id string), onMenu func(id string, pos fyne.Position)) *queryRow {
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
		qr.onTapped(qr.nodeID)
	}
}

func (qr *queryRow) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(container.New(rowLayout{}, qr.ct))
}

func (qr *queryRow) TappedSecondary(e *fyne.PointEvent) {
	if qr.onMenu != nil && qr.nodeID != "" {
		qr.onMenu(qr.nodeID, e.AbsolutePosition)
	}
}
