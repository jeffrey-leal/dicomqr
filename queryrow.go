package main

import (
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// queryRow is the canvas object used for each row in the results tree.
// Mirrors treeRow from dicomhdr: right-click context menu, hover tooltip.
type queryRow struct {
	widget.BaseWidget
	ct          *canvas.Text
	nodeID      string
	tooltipText string
	cv          fyne.Canvas
	hoverPos    fyne.Position
	hoverTimer  *time.Timer
	showPending bool
	tooltipPop  *widget.PopUp
	onMenu      func(id string, pos fyne.Position)
}

func newQueryRow(cv fyne.Canvas, onMenu func(id string, pos fyne.Position)) *queryRow {
	qr := &queryRow{
		ct:     canvas.NewText("", theme.Color(theme.ColorNameForeground)),
		cv:     cv,
		onMenu: onMenu,
	}
	qr.ExtendBaseWidget(qr)
	return qr
}

func (qr *queryRow) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(container.New(rowLayout{}, qr.ct))
}

func (qr *queryRow) TappedSecondary(e *fyne.PointEvent) {
	if qr.onMenu != nil && qr.nodeID != "" {
		qr.onMenu(qr.nodeID, e.AbsolutePosition)
	}
}

func (qr *queryRow) MouseIn(e *desktop.MouseEvent) {
	qr.hideTooltip()
	if qr.tooltipText == "" {
		return
	}
	qr.showPending = true
	qr.hoverPos = e.AbsolutePosition
	qr.hoverTimer = time.AfterFunc(600*time.Millisecond, func() {
		fyne.Do(func() {
			if qr.showPending {
				qr.showTooltip()
			}
		})
	})
}

func (qr *queryRow) MouseMoved(e *desktop.MouseEvent) {
	qr.hoverPos = e.AbsolutePosition
}

func (qr *queryRow) MouseOut() {
	qr.hideTooltip()
}

func (qr *queryRow) showTooltip() {
	if qr.cv == nil || qr.tooltipText == "" {
		return
	}
	lbl := widget.NewLabel(qr.tooltipText)
	lbl.TextStyle = fyne.TextStyle{Monospace: true}
	qr.tooltipPop = widget.NewPopUp(container.NewPadded(lbl), qr.cv)
	qr.tooltipPop.ShowAtPosition(fyne.NewPos(qr.hoverPos.X+12, qr.hoverPos.Y+16))
}

func (qr *queryRow) hideTooltip() {
	qr.showPending = false
	if qr.hoverTimer != nil {
		qr.hoverTimer.Stop()
		qr.hoverTimer = nil
	}
	if qr.tooltipPop != nil {
		qr.tooltipPop.Hide()
		qr.tooltipPop = nil
	}
}
