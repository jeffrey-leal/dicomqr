package main

import (
	"context"
	"fmt"
	"image/color"
	"log"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// logLevel classifies a captured line for the Activity Log's step-wise view
// filter. Lower values are more severe; a view level shows every entry at or
// below its threshold. The filter applies at display time only — the ring,
// dicom.log, and stderr always capture everything, so switching to a more
// verbose view retroactively reveals the full history of a problem that
// already happened.
type logLevel int

const (
	logLevelError logLevel = iota
	logLevelWarn
	logLevelInfo
	logLevelProto // untagged lines — DICOM protocol chatter from the network library
)

// logError/logWarn/logInfo tag a log line with its severity, written between
// the log package's timestamp and the message (e.g. "12:00:00.000000 [E] …").
// Lines logged without a tag — the netdicom protocol traffic, which shares
// the same funnel — classify as logLevelProto; every user-relevant failure
// has its own tagged app-level line by design, so nothing important hides at
// the protocol level.
func logError(format string, args ...any) { log.Printf("[E] "+format, args...) }
func logWarn(format string, args ...any)  { log.Printf("[W] "+format, args...) }
func logInfo(format string, args ...any)  { log.Printf("[I] "+format, args...) }

// markerLevel reads a severity tag at the start of s.
func markerLevel(s string) (logLevel, bool) {
	if len(s) >= 3 && s[0] == '[' && s[2] == ']' {
		switch s[1] {
		case 'E':
			return logLevelError, true
		case 'W':
			return logLevelWarn, true
		case 'I':
			return logLevelInfo, true
		}
	}
	return 0, false
}

// classifyLogLine derives a line's severity: a tag at the start of the line
// (direct ring writes) or immediately after the first space (after the log
// package's timestamp). Untagged lines are protocol chatter, except a
// PANIC/FATAL backstop so an exceptional line can never hide below the
// errors-only view regardless of where it came from.
func classifyLogLine(line string) logLevel {
	if lv, ok := markerLevel(line); ok {
		return lv
	}
	if i := strings.IndexByte(line, ' '); i >= 0 {
		if lv, ok := markerLevel(line[i+1:]); ok {
			return lv
		}
	}
	if strings.Contains(line, "PANIC") || strings.Contains(line, "FATAL") {
		return logLevelError
	}
	return logLevelProto
}

// logEntry is one captured line with its severity.
type logEntry struct {
	level logLevel
	text  string
}

// logCapture is a thread-safe circular ring buffer that implements io.Writer.
// It is wired into the standard log package so that all activity — app lines
// and DICOM protocol messages alike — is captured for the in-app Activity Log
// dialog at full verbosity, whatever the current view filter.
type logCapture struct {
	mu      sync.Mutex
	entries []logEntry
	max     int
	gen     uint64 // bumped on every mutation; lets viewers skip re-rendering an unchanged ring
}

// appLog is the package-level capture buffer; wired into setupLogFile. The
// capacity is sized so protocol chatter (by far the most voluminous level)
// cannot quickly evict error lines from the ring.
var appLog = newLogCapture(5000)

func newLogCapture(capacity int) *logCapture {
	return &logCapture{max: capacity}
}

// Write implements io.Writer. Each call may contain one or more newline-
// delimited lines; each non-empty line is appended as a separate entry. One
// call is one log message, so every line of a multi-line message (e.g. a
// panic with its stack trace) inherits the severity of its first line.
func (l *logCapture) Write(p []byte) (int, error) {
	text := strings.TrimRight(string(p), "\n")
	if text == "" {
		return len(p), nil
	}
	parts := strings.Split(text, "\n")
	level := classifyLogLine(parts[0])
	l.mu.Lock()
	for _, part := range parts {
		l.entries = append(l.entries, logEntry{level: level, text: part})
	}
	if len(l.entries) > l.max {
		l.entries = l.entries[len(l.entries)-l.max:]
	}
	l.gen++
	l.mu.Unlock()
	return len(p), nil
}

// Generation returns a counter that changes whenever the ring's content
// changes, so a periodic viewer can skip re-rendering an unchanged log.
func (l *logCapture) Generation() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.gen
}

// Entries returns a snapshot copy of all captured entries.
func (l *logCapture) Entries() []logEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	cp := make([]logEntry, len(l.entries))
	copy(cp, l.entries)
	return cp
}

// Clear empties the buffer.
func (l *logCapture) Clear() {
	l.mu.Lock()
	l.entries = nil
	l.gen++
	l.mu.Unlock()
}

// renderLog builds the Activity Log text for a view: entries at or below max,
// optionally narrowed to lines containing substr (case-insensitive).
func renderLog(entries []logEntry, max logLevel, substr string) string {
	needle := strings.ToLower(strings.TrimSpace(substr))
	var b strings.Builder
	for _, e := range entries {
		if e.level > max {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(e.text), needle) {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(e.text)
	}
	return b.String()
}

// logViewOptions is the step-wise view ladder, in escalating order of
// verbosity.
var logViewOptions = []struct {
	label string
	max   logLevel
}{
	{"Errors only", logLevelError},
	{"Errors + warnings", logLevelWarn},
	{"Activity", logLevelInfo},
	{"Everything", logLevelProto},
}

// logRow is one Activity Log line: compact canvas.Text in the shared
// rowLayout, with a right-click menu offering Copy line. It implements only
// secondary taps, so primary clicks fall through to the List's own row
// selection.
type logRow struct {
	widget.BaseWidget
	ct     *canvas.Text
	onMenu func(text string, pos fyne.Position)
}

func newLogRow(onMenu func(string, fyne.Position)) *logRow {
	r := &logRow{
		ct:     canvas.NewText("", theme.Color(theme.ColorNameForeground)),
		onMenu: onMenu,
	}
	r.ct.TextSize = theme.TextSize()
	r.ExtendBaseWidget(r)
	return r
}

func (r *logRow) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(container.New(rowLayout{}, r.ct))
}

func (r *logRow) TappedSecondary(e *fyne.PointEvent) {
	if r.onMenu != nil {
		r.onMenu(r.ct.Text, e.AbsolutePosition)
	}
}

// showLogDialog opens a resizable dialog that displays and auto-refreshes the
// in-memory activity log through a step-wise severity filter (Errors only →
// Everything) and an optional substring filter. Both apply at display time
// over the always-complete ring, so raising the level retroactively reveals
// already-captured detail. The dialog always opens at Errors only — problems
// first, verbosity on demand. The rows are shown in a virtualized List that
// renders only the visible slice, so even the full ring at Everything appears
// instantly (the dialog reads the in-memory ring, never dicom.log). A
// 1-second ticker updates the view while the dialog is open; the goroutine
// exits when the Close button is pressed.
// listAtBottom reports whether list is scrolled to its last row, leaving the
// scroll position as it found it. widget.List exposes its offset but not its
// content height, so the bottom is measured: jump there, read the offset, and
// put the old one back — one UI-goroutine turn, so nothing is drawn in
// between. Measuring rather than remembering the last bottom keeps the answer
// right after the window is resized. A list shorter than its viewport is
// always at its bottom.
func listAtBottom(list *widget.List) bool {
	cur := list.GetScrollOffset()
	list.ScrollToBottom()
	if bottom := list.GetScrollOffset(); cur < bottom-1 {
		list.ScrollToOffset(cur)
		return false
	}
	return true
}

func showLogDialog(a fyne.App, parent fyne.Window) {
	if raiseOwnedWindow("activity-log") {
		return
	}
	// w is the log's own window, assigned as it opens at the foot of this
	// function; the clipboard and right-click menu below belong to it. All of
	// them run from user interaction, long after the assignment.
	var w fyne.Window
	// Rows are compact logRow widgets (canvas.Text in the shared rowLayout —
	// the results trees' styling) and the List's row separators are hidden,
	// together restoring the dense line spacing of the old text view.
	// Right-clicking a row offers Copy line; Copy Shown below covers bulk
	// extraction.
	onRowMenu := func(text string, pos fyne.Position) {
		if text == "" {
			return
		}
		item := fyne.NewMenuItem("Copy line", func() { w.Clipboard().SetContent(text) })
		widget.NewPopUpMenu(fyne.NewMenu("", item), w.Canvas()).ShowAtPosition(pos)
	}
	var shown []logEntry
	list := widget.NewList(
		func() int { return len(shown) },
		func() fyne.CanvasObject { return newLogRow(onRowMenu) },
		func(i widget.ListItemID, o fyne.CanvasObject) {
			if i >= len(shown) {
				return
			}
			r := o.(*logRow)
			r.ct.Text = shown[i].text
			r.ct.Refresh()
		},
	)
	list.HideSeparators = true
	minSize := canvas.NewRectangle(color.Transparent)
	minSize.SetMinSize(fyne.NewSize(820, 420))
	listBox := container.NewStack(minSize, list)

	labels := make([]string, len(logViewOptions))
	for i, o := range logViewOptions {
		labels[i] = o.label
	}
	viewIdx := 0 // always open at Errors only
	levelSelect := widget.NewSelect(labels, nil)
	levelSelect.SetSelectedIndex(viewIdx)

	filterEntry := widget.NewEntry()
	filterEntry.SetPlaceHolder("Filter (substring, e.g. scp: or a UID)…")

	countsLbl := widget.NewLabel("")
	pausedLbl := widget.NewLabel("Paused while you read earlier lines — scroll to the bottom to follow new ones")
	pausedLbl.TextStyle = fyne.TextStyle{Italic: true}
	pausedLbl.Hide()

	atBottom := func() bool { return listAtBottom(list) }

	// The 1-second ticker calls refresh unconditionally, so it must cost
	// nothing while the log is quiet: skip everything unless the ring's
	// generation or one of the view controls actually changed. When something
	// did change, the work is a filter pass over in-memory entries plus a
	// virtualized List refresh — only the visible rows are ever laid out.
	//
	// New lines are followed only from the bottom. Scrolled up, the view is
	// held exactly as it is — not just the scroll position: the ring drops its
	// oldest entries as new ones arrive, so even a kept offset would slide
	// different lines under the reader. It resumes once the reader is back at
	// the bottom. force (Refresh, Clear) and a change of level or filter are
	// deliberate, so they always refresh and jump to the newest line.
	lastGen := ^uint64(0)
	lastIdx, lastFilter := -1, "\x00"
	refresh := func(force bool) {
		gen := appLog.Generation()
		viewChanged := viewIdx != lastIdx || filterEntry.Text != lastFilter
		if gen == lastGen && !viewChanged && !force {
			return
		}

		entries := appLog.Entries()
		var nE, nW, nI, nP int
		for _, e := range entries {
			switch e.level {
			case logLevelError:
				nE++
			case logLevelWarn:
				nW++
			case logLevelInfo:
				nI++
			default:
				nP++
			}
		}
		countsLbl.SetText(fmt.Sprintf("errors %d · warnings %d · activity %d · protocol detail %d", nE, nW, nI, nP))

		if !force && !viewChanged && !atBottom() {
			// lastGen is left as it was, so the first tick back at the
			// bottom catches up on everything that arrived meanwhile.
			pausedLbl.Show()
			return
		}
		pausedLbl.Hide()
		lastGen, lastIdx, lastFilter = gen, viewIdx, filterEntry.Text

		max := logViewOptions[viewIdx].max
		needle := strings.ToLower(strings.TrimSpace(filterEntry.Text))
		filtered := make([]logEntry, 0, len(entries))
		for _, e := range entries {
			if e.level > max {
				continue
			}
			if needle != "" && !strings.Contains(strings.ToLower(e.text), needle) {
				continue
			}
			filtered = append(filtered, e)
		}
		shown = filtered
		list.Refresh()
		list.ScrollToBottom()
	}
	refresh(true)

	levelSelect.OnChanged = func(string) {
		if idx := levelSelect.SelectedIndex(); idx >= 0 {
			viewIdx = idx
		}
		refresh(false)
	}
	filterEntry.OnChanged = func(string) { refresh(false) }

	ctx, cancel := context.WithCancel(context.Background())

	closeBtn := widget.NewButton("Close", func() {
		cancel()
	})
	refreshBtn := widget.NewButton("Refresh", func() { fyne.Do(func() { refresh(true) }) })
	copyBtn := widget.NewButton("Copy Shown", func() {
		w.Clipboard().SetContent(renderLog(appLog.Entries(), logViewOptions[viewIdx].max, filterEntry.Text))
	})
	clearBtn := widget.NewButton("Clear", func() {
		appLog.Clear()
		fyne.Do(func() { refresh(true) })
	})

	filterBar := container.NewBorder(nil, nil, levelSelect, countsLbl, filterEntry)

	content := container.NewBorder(
		filterBar,
		container.NewVBox(
			widget.NewSeparator(),
			container.NewHBox(refreshBtn, copyBtn, clearBtn, pausedLbl, layout.NewSpacer(), closeBtn),
		),
		nil, nil,
		listBox,
	)

	closeBtn.OnTapped = func() { w.Close() }

	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				fyne.Do(func() { refresh(false) })
			}
		}
	}()

	// Deliberately not blocking: the log is for consulting while working, so
	// the window behind it stays live. OnClosed stops the refresh ticker on
	// every route out — previously only the Close button did, so dismissing
	// the panel any other way left the goroutine running for the session.
	w = openOwnedWindow(a, windowSpec{
		Key:      "activity-log",
		Title:    "Activity Log",
		Size:     fyne.NewSize(880, 580),
		Parent:   parent,
		OnClosed: cancel,
	}, func(fyne.Window) fyne.CanvasObject {
		return container.NewPadded(content)
	})
}
