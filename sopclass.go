package main

import (
	"fmt"
	"strings"
)

// The ignoresopclass profile filter skips a file by its SOP Class UID
// (0008,0016). It exists because the objects a de-identification most needs to
// drop — scanned documents, dose and protocol pages, saved screens — are
// Secondary Capture objects that carry the study's imaging modality, so
// neither the ImageType nor the Modality filter ever sees them. Surveyed over
// 55,920 files in 25 patients' downloads (2026-09-03): every such series was
// a Secondary Capture object labelled CT, MR, NM, PT or US, and no natively
// classed image series looked like a document. One kiosk scan carried no
// ImageType at all, which is why the filter keys on the SOP class and not on
// ImageType SECONDARY (which would also drop MPR reformats).
//
// sopClassNames names the storage classes a user is likely to list, for the
// editor's hint line, the Modification dialog and the Activity Log. It is not
// a validation authority: any syntactically valid UID is accepted, since
// private SOP classes exist, and an unknown one is shown as its UID.
var sopClassNames = map[string]string{
	"1.2.840.10008.5.1.4.1.1.7":        "Secondary Capture Image",
	"1.2.840.10008.5.1.4.1.1.7.1":      "Multi-frame Single Bit Secondary Capture",
	"1.2.840.10008.5.1.4.1.1.7.2":      "Multi-frame Grayscale Byte Secondary Capture",
	"1.2.840.10008.5.1.4.1.1.7.3":      "Multi-frame Grayscale Word Secondary Capture",
	"1.2.840.10008.5.1.4.1.1.7.4":      "Multi-frame True Color Secondary Capture",
	"1.2.840.10008.5.1.4.1.1.104.1":    "Encapsulated PDF",
	"1.2.840.10008.5.1.4.1.1.104.2":    "Encapsulated CDA",
	"1.2.840.10008.5.1.4.1.1.104.3":    "Encapsulated STL",
	"1.2.840.10008.5.1.4.1.1.1":        "Computed Radiography Image",
	"1.2.840.10008.5.1.4.1.1.1.1":      "Digital X-Ray Image (Presentation)",
	"1.2.840.10008.5.1.4.1.1.1.1.1":    "Digital X-Ray Image (Processing)",
	"1.2.840.10008.5.1.4.1.1.1.2":      "Digital Mammography X-Ray Image (Presentation)",
	"1.2.840.10008.5.1.4.1.1.1.2.1":    "Digital Mammography X-Ray Image (Processing)",
	"1.2.840.10008.5.1.4.1.1.2":        "CT Image",
	"1.2.840.10008.5.1.4.1.1.2.1":      "Enhanced CT Image",
	"1.2.840.10008.5.1.4.1.1.3.1":      "Ultrasound Multi-frame Image",
	"1.2.840.10008.5.1.4.1.1.4":        "MR Image",
	"1.2.840.10008.5.1.4.1.1.4.1":      "Enhanced MR Image",
	"1.2.840.10008.5.1.4.1.1.6.1":      "Ultrasound Image",
	"1.2.840.10008.5.1.4.1.1.11.1":     "Grayscale Softcopy Presentation State",
	"1.2.840.10008.5.1.4.1.1.11.2":     "Color Softcopy Presentation State",
	"1.2.840.10008.5.1.4.1.1.12.1":     "X-Ray Angiographic Image",
	"1.2.840.10008.5.1.4.1.1.12.2":     "X-Ray Radiofluoroscopic Image",
	"1.2.840.10008.5.1.4.1.1.20":       "Nuclear Medicine Image",
	"1.2.840.10008.5.1.4.1.1.66":       "Raw Data",
	"1.2.840.10008.5.1.4.1.1.77.1.1":   "VL Endoscopic Image",
	"1.2.840.10008.5.1.4.1.1.77.1.4":   "VL Photographic Image",
	"1.2.840.10008.5.1.4.1.1.88.11":    "Basic Text SR",
	"1.2.840.10008.5.1.4.1.1.88.22":    "Enhanced SR",
	"1.2.840.10008.5.1.4.1.1.88.33":    "Comprehensive SR",
	"1.2.840.10008.5.1.4.1.1.88.59":    "Key Object Selection Document",
	"1.2.840.10008.5.1.4.1.1.88.67":    "X-Ray Radiation Dose SR",
	"1.2.840.10008.5.1.4.1.1.128":      "Positron Emission Tomography Image",
	"1.2.840.10008.5.1.4.1.1.130":      "Enhanced PET Image",
	"1.2.840.10008.5.1.4.1.1.481.1":    "RT Image",
	"1.2.840.10008.5.1.4.1.1.481.2":    "RT Dose",
	"1.2.840.10008.5.1.4.1.1.481.3":    "RT Structure Set",
	"1.2.840.10008.5.1.4.1.1.481.5":    "RT Plan",
	"1.2.840.10008.5.1.4.1.1.9.1.1":    "12-lead ECG Waveform",
	"1.2.840.10008.5.1.4.1.1.9.1.2":    "General ECG Waveform",
	"1.2.840.10008.5.1.4.1.1.9.1.3":    "Ambulatory ECG Waveform",
	"1.2.840.10008.5.1.4.1.1.14.1":     "Intravascular OCT Image (Presentation)",
	"1.2.840.10008.5.1.4.1.1.77.1.5.1": "Ophthalmic Photography 8 Bit Image",
}

// secondaryCaptureSOPClasses is the family the standard defines for images
// converted from a non-DICOM source (PS3.3 A.8): the single-frame class and
// its four multi-frame variants. It is what the manual and the editor's hint
// recommend listing; the filter itself takes any UID.
var secondaryCaptureSOPClasses = []string{
	"1.2.840.10008.5.1.4.1.1.7",
	"1.2.840.10008.5.1.4.1.1.7.1",
	"1.2.840.10008.5.1.4.1.1.7.2",
	"1.2.840.10008.5.1.4.1.1.7.3",
	"1.2.840.10008.5.1.4.1.1.7.4",
}

// sopClassName returns the well-known name of a SOP Class UID, or "" when the
// table does not know it.
func sopClassName(uid string) string {
	return sopClassNames[strings.TrimSpace(uid)]
}

// describeSOPClass renders a UID as "Name (uid)" when the name is known and
// as the bare UID otherwise, so a message never loses the value that was
// actually matched.
func describeSOPClass(uid string) string {
	uid = strings.TrimSpace(uid)
	if name := sopClassName(uid); name != "" {
		return name + " (" + uid + ")"
	}
	return uid
}

// sopClassListSummary names every entry of an ignoresopclass list for
// display: known classes by name, unknown ones as their UID. Entries are
// shown in the order stored; blanks are skipped. Returns "" for an empty
// list.
func sopClassListSummary(uids []string) string {
	var parts []string
	for _, u := range uids {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		if name := sopClassName(u); name != "" {
			parts = append(parts, name)
		} else {
			parts = append(parts, u)
		}
	}
	return strings.Join(parts, "; ")
}

// isValidUID reports whether s has the shape of a DICOM UID: dot-separated
// runs of digits, at most 64 characters. Deliberately lenient about leading
// zeros, which the standard forbids but shipped software emits — a filter
// that refused the UID a vendor actually wrote could never match it.
func isValidUID(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, comp := range strings.Split(s, ".") {
		if comp == "" {
			return false
		}
		for _, r := range comp {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

// validateSOPClassUIDs trims an ignoresopclass list, drops blank entries and
// rejects the first entry that is not a UID. Returns nil (not an empty slice)
// when nothing remains, so an untouched profile round-trips byte-identical
// through the editor's change detection.
func validateSOPClassUIDs(list []string) ([]string, error) {
	var out []string
	for _, v := range list {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if !isValidUID(v) {
			return nil, fmt.Errorf("ignore SOP classes: %q is not a UID (digits and dots, e.g. 1.2.840.10008.5.1.4.1.1.7)", v)
		}
		out = append(out, v)
	}
	return out, nil
}

// sopClassHintText names each entry of a list as typed into the editor:
// known classes by name, a valid but unknown UID as "unknown class", and
// anything that is not a UID as such — so the mistake is visible before
// Apply rejects it.
func sopClassHintText(entries []string) string {
	var parts []string
	for _, e := range entries {
		e = strings.TrimSpace(e)
		switch {
		case e == "":
		case !isValidUID(e):
			parts = append(parts, e+": not a UID")
		case sopClassName(e) != "":
			parts = append(parts, sopClassName(e))
		default:
			parts = append(parts, e+": unknown class")
		}
	}
	return strings.Join(parts, "; ")
}

// matchesSOPClass reports whether the SOP Class UID read from a file is in
// the list. Values are compared trimmed: a UID read from a file can carry
// the even-length padding the encoding requires.
func matchesSOPClass(value string, list []string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	for _, u := range list {
		if strings.TrimSpace(u) == value {
			return true
		}
	}
	return false
}
