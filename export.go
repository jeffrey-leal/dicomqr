package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
)

func exportToCSV(path string, rows []ExportRow) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer f.Close()
	w := csv.NewWriter(f)
	w.Write([]string{
		"PatientName", "PatientID", "StudyDate", "StudyDescription",
		"AccessionNumber", "Modalities", "StudyInstanceUID",
		"SeriesInstanceUID", "SeriesNumber", "Modality", "NumInstances",
	})
	for _, r := range rows {
		w.Write([]string{
			r.PatientName, r.PatientID, r.StudyDate, r.StudyDesc,
			r.Accession, r.Modalities, r.StudyUID,
			r.SeriesUID, r.SeriesNumber, r.Modality,
			fmt.Sprintf("%d", r.NumInstances),
		})
	}
	w.Flush()
	return w.Error()
}

func exportToJSON(path string, rows []ExportRow) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(rows)
}
