package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
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

// logViewOptions maps the persisted logViewLevel setting to the step-wise
// view choices, in escalating order of verbosity.
var logViewOptions = []struct {
	key   string // value stored in settings.json
	label string
	max   logLevel
}{
	{"errors", "Errors only", logLevelError},
	{"warnings", "Errors + warnings", logLevelWarn},
	{"activity", "Activity", logLevelInfo},
	{"everything", "Everything", logLevelProto},
}

// logViewIndex resolves a persisted logViewLevel value to an option index,
// defaulting to Activity for empty or unknown values.
func logViewIndex(key string) int {
	for i, o := range logViewOptions {
		if o.key == key {
			return i
		}
	}
	return 2 // "activity"
}

// readOnlyEntry is a multi-line Entry that stays enabled (so text renders in
// the normal foreground colour rather than the faded disabled shade) but
// swallows every editing input. Mouse selection, caret navigation, Select All
// and Copy still work.
type readOnlyEntry struct {
	widget.Entry
}

func newReadOnlyEntry() *readOnlyEntry {
	e := &readOnlyEntry{}
	e.MultiLine = true
	e.Wrapping = fyne.TextWrapOff
	e.ExtendBaseWidget(e)
	return e
}

func (e *readOnlyEntry) TypedRune(_ rune) {}

func (e *readOnlyEntry) TypedKey(ev *fyne.KeyEvent) {
	switch ev.Name {
	case fyne.KeyBackspace, fyne.KeyDelete, fyne.KeyReturn, fyne.KeyEnter, fyne.KeyTab:
		return
	}
	e.Entry.TypedKey(ev)
}

func (e *readOnlyEntry) TypedShortcut(s fyne.Shortcut) {
	switch s.(type) {
	case *fyne.ShortcutCopy, *fyne.ShortcutSelectAll:
		e.Entry.TypedShortcut(s)
	}
}

// showLogDialog opens a resizable dialog that displays and auto-refreshes the
// in-memory activity log through a step-wise severity filter (Errors only →
// Everything) and an optional substring filter. Both apply at display time
// over the always-complete ring, so raising the level retroactively reveals
// already-captured detail. The chosen level persists as logViewLevel. A
// 1-second ticker updates the view while the dialog is open; the goroutine
// exits when the Close button is pressed.
func showLogDialog(w fyne.Window, cfg *Settings) {
	entry := newReadOnlyEntry()

	scroll := container.NewVScroll(entry)
	scroll.SetMinSize(fyne.NewSize(820, 420))

	labels := make([]string, len(logViewOptions))
	for i, o := range logViewOptions {
		labels[i] = o.label
	}
	viewIdx := logViewIndex(cfg.LogViewLevel)
	levelSelect := widget.NewSelect(labels, nil)
	levelSelect.SetSelectedIndex(viewIdx)

	filterEntry := widget.NewEntry()
	filterEntry.SetPlaceHolder("Filter (substring, e.g. scp: or a UID)…")

	countsLbl := widget.NewLabel("")

	// The 1-second ticker calls refresh unconditionally, so it must cost
	// nothing while the log is quiet: re-setting a several-thousand-line Entry
	// every tick kept the whole app busy. Skip everything unless the ring's
	// generation or one of the view controls actually changed.
	lastGen := ^uint64(0)
	lastIdx, lastFilter := -1, "\x00"
	refresh := func() {
		gen := appLog.Generation()
		if gen == lastGen && viewIdx == lastIdx && filterEntry.Text == lastFilter {
			return
		}
		lastGen, lastIdx, lastFilter = gen, viewIdx, filterEntry.Text

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
		entry.SetText(renderLog(entries, logViewOptions[viewIdx].max, filterEntry.Text))
		scroll.ScrollToBottom()
	}
	refresh()

	levelSelect.OnChanged = func(string) {
		viewIdx = levelSelect.SelectedIndex()
		if viewIdx < 0 {
			viewIdx = logViewIndex("")
		}
		refresh()
		if key := logViewOptions[viewIdx].key; key != cfg.LogViewLevel {
			cfg.LogViewLevel = key
			saveSettings(*cfg)
		}
	}
	filterEntry.OnChanged = func(string) { refresh() }

	ctx, cancel := context.WithCancel(context.Background())

	closeBtn := widget.NewButton("Close", func() {
		cancel()
	})
	refreshBtn := widget.NewButton("Refresh", func() { fyne.Do(refresh) })
	copyBtn := widget.NewButton("Copy Shown", func() {
		w.Clipboard().SetContent(renderLog(appLog.Entries(), logViewOptions[viewIdx].max, filterEntry.Text))
	})
	clearBtn := widget.NewButton("Clear", func() {
		appLog.Clear()
		fyne.Do(refresh)
	})

	filterBar := container.NewBorder(nil, nil, levelSelect, countsLbl, filterEntry)

	content := container.NewBorder(
		filterBar,
		container.NewVBox(
			widget.NewSeparator(),
			container.NewHBox(refreshBtn, copyBtn, clearBtn, layout.NewSpacer(), closeBtn),
		),
		nil, nil,
		scroll,
	)

	dlg := widget.NewModalPopUp(container.NewPadded(content), w.Canvas())
	dlg.Resize(fyne.NewSize(860, 560))

	closeBtn.OnTapped = func() {
		cancel()
		dlg.Hide()
	}

	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				fyne.Do(refresh)
			}
		}
	}()

	dlg.Show()
}
