package main

import (
	"context"
	"fmt"
	"image/color"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/widget"
)

// worklistColumns defines the ordered display columns for the worklist table.
var worklistColumns = []struct {
	header string
	width  float32
}{
	{"Patient", 170},
	{"MRN", 110},
	{"Accession", 120},
	{"Date", 80},
	{"Time", 70},
	{"Mod", 50},
	{"Procedure", 200},
	{"Station", 110},
}

// worklistCellValue returns the string value for the given (row, col) pair.
func worklistCellValue(results []WorklistResult, row, col int) string {
	if row < 0 || row >= len(results) {
		return ""
	}
	r := results[row]
	switch col {
	case 0:
		return r.PatientName
	case 1:
		return r.PatientID
	case 2:
		return r.AccessionNumber
	case 3:
		return formatWorklistDate(r.ScheduledDate)
	case 4:
		return formatWorklistTime(r.ScheduledTime)
	case 5:
		return r.Modality
	case 6:
		if r.ProcedureStepDesc != "" {
			return r.ProcedureStepDesc
		}
		return r.RequestedProcedureDesc
	case 7:
		return r.ScheduledStation
	}
	return ""
}

// formatWorklistDate converts a YYYYMMDD DICOM date to YYYY-MM-DD for display.
func formatWorklistDate(s string) string {
	if len(s) == 8 {
		return s[0:4] + "-" + s[4:6] + "-" + s[6:8]
	}
	return s
}

// formatWorklistTime converts a HHMMSS DICOM time to HH:MM for display.
func formatWorklistTime(s string) string {
	if len(s) >= 4 {
		return s[0:2] + ":" + s[2:4]
	}
	return s
}

// buildWorklistContent constructs the Worklist tab.
// Returns the tab content and a refresh func that must be called after the
// server profiles list changes (e.g. after Preferences apply).
func buildWorklistContent(w fyne.Window, cfg *Settings) (fyne.CanvasObject, func()) {
	var results []WorklistResult
	var cancelQuery context.CancelFunc

	statusLbl := widget.NewLabel("Select a worklist server and click Query Worklist.")

	// ── Table ──────────────────────────────────────────────────────────────────
	var table *widget.Table
	table = widget.NewTable(
		func() (int, int) { return len(results), len(worklistColumns) },
		func() fyne.CanvasObject {
			lbl := widget.NewLabel("")
			lbl.Truncation = fyne.TextTruncateEllipsis
			return lbl
		},
		func(id widget.TableCellID, cell fyne.CanvasObject) {
			cell.(*widget.Label).SetText(worklistCellValue(results, id.Row, id.Col))
		},
	)
	for i, col := range worklistColumns {
		table.SetColumnWidth(i, col.width)
	}

	// ── Header row (fixed, matches table column widths) ─────────────────────
	headerObjs := make([]fyne.CanvasObject, len(worklistColumns))
	for i, col := range worklistColumns {
		lbl := widget.NewLabelWithStyle(col.header, fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
		lbl.Truncation = fyne.TextTruncateEllipsis
		sizer := canvas.NewRectangle(color.Transparent)
		sizer.SetMinSize(fyne.NewSize(col.width, 0))
		headerObjs[i] = container.NewStack(sizer, lbl)
	}
	header := container.NewHBox(headerObjs...)

	// ── Worklist server selector ───────────────────────────────────────────────
	profileNames := func() []string {
		names := make([]string, len(cfg.Profiles))
		for i, p := range cfg.Profiles {
			names[i] = p.Name
		}
		return names
	}
	profileSelect := widget.NewSelect(profileNames(), nil)
	if len(cfg.Profiles) > 0 {
		profileSelect.SetSelectedIndex(0)
	}

	// ── Query fields ───────────────────────────────────────────────────────────
	patientNameEntry := widget.NewEntry()
	patientNameEntry.SetPlaceHolder("DOE^JOHN or DOE*")
	patientIDEntry := widget.NewEntry()
	patientIDEntry.SetPlaceHolder("MRN")
	accessionEntry := widget.NewEntry()
	accessionEntry.SetPlaceHolder("Accession No.")

	dateEntry := widget.NewDateEntry()
	dateEntry.Validator = nil // allow empty (= no date filter)

	todayCheck := widget.NewCheck("Today only", nil)
	todayCheck.OnChanged = func(checked bool) {
		if checked {
			today := time.Now()
			dateEntry.SetDate(&today)
			dateEntry.Disable()
		} else {
			dateEntry.Enable()
		}
	}
	// Initialise to today, then disable — matches the default checked state below.
	today := time.Now()
	dateEntry.SetDate(&today)
	dateEntry.Disable()
	todayCheck.SetChecked(true)

	modalitySelect := widget.NewSelect(
		[]string{"(any)", "CT", "MR", "PT", "NM", "US", "CR", "DX", "XA", "RF"},
		nil,
	)
	modalitySelect.SetSelected("(any)")

	var doQuery func()

	// queryGen numbers the table's contents (UI goroutine only), as resultsGen
	// does for the Query Results tree: Cancel, Clear and every new query bump
	// it, and a query posts its results only if the number is unchanged — a
	// query cancelled after it had collected its matches used to fill the
	// table anyway.
	var queryGen int
	queryBtn := widget.NewButton("Query Worklist", func() { doQuery() })
	cancelBtn := widget.NewButton("Cancel", nil)
	cancelBtn.Disable()
	stopQuery := func() {
		if cancelQuery != nil {
			cancelQuery()
		}
		queryGen++
		queryBtn.Enable()
		cancelBtn.Disable()
	}
	cancelBtn.OnTapped = func() {
		stopQuery()
		statusLbl.SetText("Worklist query cancelled.")
	}

	clearBtn := widget.NewButton("Clear", func() {
		stopQuery()
		patientNameEntry.SetText("")
		patientIDEntry.SetText("")
		accessionEntry.SetText("")
		modalitySelect.SetSelected("(any)")
		todayCheck.SetChecked(true)
		results = nil
		table.Refresh()
		statusLbl.SetText("Ready.")
	})

	patientNameEntry.OnSubmitted = func(_ string) { doQuery() }
	patientIDEntry.OnSubmitted = func(_ string) { doQuery() }
	accessionEntry.OnSubmitted = func(_ string) { doQuery() }

	doQuery = func() {
		if len(cfg.Profiles) == 0 {
			dialog.ShowInformation("No profiles",
				"Add a server profile in File → Preferences before querying the worklist.", w)
			return
		}
		var chosen *ServerProfile
		for i := range cfg.Profiles {
			if cfg.Profiles[i].Name == profileSelect.Selected {
				chosen = &cfg.Profiles[i]
				break
			}
		}
		if chosen == nil {
			return
		}

		scheduledDate := ""
		if todayCheck.Checked {
			scheduledDate = time.Now().Format("20060102")
		} else if dateEntry.Date != nil {
			scheduledDate = dateEntry.Date.Format("20060102")
		}

		modality := modalitySelect.Selected
		if modality == "(any)" {
			modality = ""
		}

		wildcard := func(s string) string {
			s = strings.TrimSpace(s)
			if s == "" || strings.HasSuffix(s, "*") {
				return s
			}
			return s + "*"
		}

		params := map[string]string{
			"PatientName":     wildcard(patientNameEntry.Text),
			"PatientID":       wildcard(patientIDEntry.Text),
			"AccessionNumber": wildcard(accessionEntry.Text),
			"ScheduledDate":   scheduledDate,
			"Modality":        modality,
		}

		stopQuery() // a query still running is superseded
		ctx, cancel := context.WithCancel(context.Background())
		cancelQuery = cancel
		gen := queryGen

		results = nil
		table.Refresh()
		statusLbl.SetText("Querying worklist…")
		queryBtn.Disable()
		cancelBtn.Enable()

		cl := NewDicomClient(*chosen, cfg.LocalAETitle)

		go func() {
			defer cancel()
			ch, err := cl.FindWorklist(ctx, params)
			if err != nil {
				fyne.Do(func() {
					if gen != queryGen {
						return
					}
					statusLbl.SetText("Worklist query error: " + err.Error())
					queryBtn.Enable()
					cancelBtn.Disable()
				})
				return
			}

			var collected []WorklistResult
			var firstErr error
			for r := range ch {
				if r.Err != nil {
					if firstErr == nil {
						firstErr = r.Err
					}
					continue
				}
				collected = append(collected, r)
			}

			fyne.Do(func() {
				if gen != queryGen {
					return // cancelled, cleared or superseded meanwhile
				}
				results = collected
				table.Refresh()
				queryBtn.Enable()
				cancelBtn.Disable()
				switch {
				case firstErr != nil && len(collected) == 0:
					statusLbl.SetText("Worklist query error: " + firstErr.Error())
				case firstErr != nil:
					// A server that stopped part-way: say the list is incomplete
					// rather than present it as the whole worklist.
					statusLbl.SetText(fmt.Sprintf("%s — incomplete: %v",
						plural(len(collected), "worklist item", "worklist items"), firstErr))
				default:
					statusLbl.SetText(plural(len(collected), "worklist item", "worklist items"))
				}
			})
		}()
	}

	// ── Selected-row copy helper ────────────────────────────────────────────────
	var selectedRow int = -1
	table.OnSelected = func(id widget.TableCellID) {
		selectedRow = id.Row
	}
	table.OnUnselected = func(_ widget.TableCellID) {
		selectedRow = -1
	}

	copyAccBtn := widget.NewButton("Copy Accession", func() {
		if selectedRow >= 0 && selectedRow < len(results) {
			w.Clipboard().SetContent(results[selectedRow].AccessionNumber)
		}
	})
	copyPatBtn := widget.NewButton("Copy Patient", func() {
		if selectedRow >= 0 && selectedRow < len(results) {
			w.Clipboard().SetContent(results[selectedRow].PatientName)
		}
	})

	// ── Layout ─────────────────────────────────────────────────────────────────
	serverRow := container.NewBorder(nil, nil,
		widget.NewLabel("Worklist server:"), nil,
		profileSelect,
	)

	queryBar := container.NewVBox(
		serverRow,
		widget.NewSeparator(),
		container.New(layout.NewFormLayout(),
			widget.NewLabel("Patient Name"), patientNameEntry,
			widget.NewLabel("MRN"), patientIDEntry,
			widget.NewLabel("Accession"), accessionEntry,
			widget.NewLabel("Modality"), modalitySelect,
			widget.NewLabel("Scheduled date"), container.NewBorder(nil, nil, todayCheck, nil, dateEntry),
		),
		container.NewHBox(queryBtn, cancelBtn, clearBtn),
	)

	bottomBar := container.NewVBox(
		widget.NewSeparator(),
		container.NewHBox(copyAccBtn, copyPatBtn, layout.NewSpacer()),
		statusLbl,
	)

	tableWithHeader := container.NewBorder(header, nil, nil, nil, table)

	refresh := func() {
		profileSelect.Options = profileNames()
		profileSelect.Refresh()
	}

	return container.NewBorder(queryBar, bottomBar, nil, nil, tableWithHeader), refresh
}
