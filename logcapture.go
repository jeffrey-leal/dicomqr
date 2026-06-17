package main

import (
	"context"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/widget"
)

// logCapture is a thread-safe circular ring buffer that implements io.Writer.
// It is wired into the standard log package so that all DICOM protocol messages
// are captured for the in-app Activity Log dialog.
type logCapture struct {
	mu    sync.Mutex
	lines []string
	max   int
}

// appLog is the package-level capture buffer; wired into setupLogFile.
var appLog = newLogCapture(500)

func newLogCapture(capacity int) *logCapture {
	return &logCapture{max: capacity}
}

// Write implements io.Writer. Each call may contain one or more newline-delimited
// log lines; each non-empty line is appended as a separate entry.
func (l *logCapture) Write(p []byte) (int, error) {
	text := strings.TrimRight(string(p), "\n")
	if text == "" {
		return len(p), nil
	}
	parts := strings.Split(text, "\n")
	l.mu.Lock()
	l.lines = append(l.lines, parts...)
	if len(l.lines) > l.max {
		l.lines = l.lines[len(l.lines)-l.max:]
	}
	l.mu.Unlock()
	return len(p), nil
}

// Lines returns a snapshot copy of all captured lines.
func (l *logCapture) Lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	cp := make([]string, len(l.lines))
	copy(cp, l.lines)
	return cp
}

// Clear empties the buffer.
func (l *logCapture) Clear() {
	l.mu.Lock()
	l.lines = nil
	l.mu.Unlock()
}

// showLogDialog opens a resizable dialog that displays and auto-refreshes the
// in-memory activity log. A 1-second ticker updates the view while it is open;
// the goroutine exits when the Close button is pressed.
func showLogDialog(w fyne.Window) {
	entry := widget.NewMultiLineEntry()
	entry.Disable()
	entry.Wrapping = fyne.TextWrapOff

	scroll := container.NewVScroll(entry)
	scroll.SetMinSize(fyne.NewSize(820, 440))

	refresh := func() {
		lines := appLog.Lines()
		entry.SetText(strings.Join(lines, "\n"))
		scroll.ScrollToBottom()
	}
	refresh()

	ctx, cancel := context.WithCancel(context.Background())

	closeBtn := widget.NewButton("Close", func() {
		cancel()
	})
	refreshBtn := widget.NewButton("Refresh", func() { fyne.Do(refresh) })
	copyBtn := widget.NewButton("Copy All", func() {
		w.Clipboard().SetContent(strings.Join(appLog.Lines(), "\n"))
	})
	clearBtn := widget.NewButton("Clear", func() {
		appLog.Clear()
		fyne.Do(func() { entry.SetText("") })
	})

	content := container.NewBorder(
		nil,
		container.NewVBox(
			widget.NewSeparator(),
			container.NewHBox(refreshBtn, copyBtn, clearBtn, layout.NewSpacer(), closeBtn),
		),
		nil, nil,
		scroll,
	)

	dlg := widget.NewModalPopUp(container.NewPadded(content), w.Canvas())
	dlg.Resize(fyne.NewSize(860, 540))

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
