package main

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// Which text fields count as carrying a value: blanks do not, a value nested
// in a request sequence does, and a sequence counts once any item holds
// something. Returned in phiTextTags order.
func TestTextFieldsWithValues(t *testing.T) {
	elems := []*sdicom.Element{
		mustTestElement(t, tag.ImageComments, []string{"   "}),
		mustTestElement(t, tag.PatientComments, []string{"prefers Jane"}),
		mustTestElement(t, tag.OtherPatientIDsSequence, [][]*sdicom.Element{{
			mustTestElement(t, tag.PatientID, []string{"OLD-42"}),
		}}),
		mustTestElement(t, tag.RequestAttributesSequence, [][]*sdicom.Element{{
			mustTestElement(t, tag.RequestedProcedureComments, []string{"call J. Doe on arrival"}),
		}}),
		mustTestElement(t, tag.StudyDescription, []string{"CT CHEST"}), // not a checked field
	}
	got := textFieldsWithValues(elems)
	want := []tag.Tag{tag.OtherPatientIDsSequence, tag.PatientComments, tag.RequestedProcedureComments}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("textFieldsWithValues = %v, want %v", got, want)
	}
}

// textRulesFor layers a per-modality override exactly as processFile does:
// override removals added, override keep cancelling a profile removal, Set
// values counted as handled.
func TestTextRulesForLayersOverride(t *testing.T) {
	p, err := compileModifyParams(ModProfile{
		Removes: []string{"0020,4000", "0010,4000"},
		Sets:    []string{"0010,21B0=NONE"},
		PerModality: map[string]ModProfile{
			"US": {Keep: []string{"0020,4000"}, Removes: []string{"0040,1400"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ct := textRulesFor(p, "CT")
	if !ct[tag.ImageComments] || !ct[tag.PatientComments] || !ct[tag.AdditionalPatientHistory] || ct[tag.RequestedProcedureComments] {
		t.Errorf("CT rules = %v", ct)
	}
	us := textRulesFor(p, "us")
	if us[tag.ImageComments] || !us[tag.PatientComments] || !us[tag.RequestedProcedureComments] {
		t.Errorf("US rules = %v (keep must cancel the profile's removal)", us)
	}
}

// The text reason in the classification, and the breakdown the dialog shows.
func TestPHITextRisk(t *testing.T) {
	h := phiHeader{text: []tag.Tag{tag.ImageComments, tag.RequestedProcedureComments}}
	if got := h.risks(phiRules{}); got != phiRiskText {
		t.Errorf("unhandled text: risks = %06b", got)
	}
	handled := map[tag.Tag]bool{tag.ImageComments: true, tag.RequestedProcedureComments: true}
	if got := h.risks(phiRules{textHandled: handled}); got != 0 {
		t.Errorf("all handled: risks = %06b", got)
	}
	files := []phiScreenFile{
		{path: "a", phiHeader: h},
		{path: "b", phiHeader: phiHeader{text: []tag.Tag{tag.ImageComments}}},
		{path: "c", skipped: true, phiHeader: phiHeader{text: []tag.Tag{tag.PatientComments}}},
	}
	f := evaluatePHIScreen(files, func(string) phiRules { return phiRules{} })
	if len(f.byRisk[phiRiskText]) != 2 {
		t.Errorf("text findings = %v", f.byRisk[phiRiskText])
	}
	if got, want := f.textTags(), []tag.Tag{tag.ImageComments, tag.RequestedProcedureComments}; !reflect.DeepEqual(got, want) {
		t.Errorf("textTags = %v, want %v (skipped file excluded)", got, want)
	}
	if b := f.textBreakdown(); !strings.Contains(b, "Image Comments (0020,4000) in 2 files") ||
		!strings.Contains(b, "(0040,1400) in 1 file") {
		t.Errorf("breakdown = %q", b)
	}
}

// The screen and the run agree: for each profile, the files the screen flags
// for text are exactly the ones the run counts as shipped with text — the
// screen judges from the header and the compiled rules, the run from the
// finished dataset, and a disagreement would make one of them lie.
func TestPHITextScreenMatchesRun(t *testing.T) {
	rootDir := t.TempDir()
	commented := filepath.Join(rootDir, "commented.dcm")
	nested := filepath.Join(rootDir, "nested.dcm")
	plain := filepath.Join(rootDir, "plain.dcm")
	usFile := filepath.Join(rootDir, "us.dcm")
	writePHIFixture(t, commented, sopCTImage, "CT", "MONOCHROME2",
		mustTestElement(t, tag.ImageComments, []string{"J. Doe follow-up"}))
	writePHIFixture(t, nested, sopCTImage, "CT", "MONOCHROME2",
		mustTestElement(t, tag.RequestAttributesSequence, [][]*sdicom.Element{{
			mustTestElement(t, tag.RequestedProcedureComments, []string{"call J. Doe"}),
		}}))
	writePHIFixture(t, plain, sopCTImage, "CT", "MONOCHROME2")
	writePHIFixture(t, usFile, "1.2.840.10008.5.1.4.1.1.6.1", "US", "MONOCHROME2",
		mustTestElement(t, tag.ImageComments, []string{"4CH view"}))
	files := []string{commented, nested, plain, usFile}
	// Every image masked, so only the text reason is in play.
	maskAll := []MaskRegion{{X: 0, Y: 0, W: 0.5, H: 0.5}}

	for _, tc := range []struct {
		name    string
		profile ModProfile
		want    []string // files with text left, by base name
	}{
		{"nothing removed", ModProfile{Sets: []string{"0010,0010=ANON"}},
			[]string{"commented.dcm", "nested.dcm", "us.dcm"}},
		{"top-level comment removed, nested one kept", ModProfile{Removes: []string{"0020,4000"}},
			[]string{"nested.dcm"}},
		{"both removed", ModProfile{Removes: []string{"0020,4000", "0040,1400"}}, nil},
		{"set values count as handled", ModProfile{Sets: []string{"0020,4000=REDACTED"}, Removes: []string{"0040,1400"}}, nil},
		{"override keep restores a field", ModProfile{
			Removes:     []string{"0020,4000", "0040,1400"},
			PerModality: map[string]ModProfile{"US": {Keep: []string{"0020,4000"}}},
		}, []string{"us.dcm"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.profile
			p.MaskRegions = maskAll
			params, err := compileModifyParams(p)
			if err != nil {
				t.Fatalf("compile: %v", err)
			}

			screened, _ := screenPHIFiles(files, params, nil, nil)
			f := evaluatePHIScreen(screened, func(m string) phiRules {
				return phiRules{regions: maskAll, textHandled: textRulesFor(params, m)}
			})
			var flagged []string
			for _, path := range f.byRisk[phiRiskText] {
				flagged = append(flagged, filepath.Base(path))
			}
			if !reflect.DeepEqual(flagged, tc.want) {
				t.Errorf("screen flagged %v, want %v", flagged, tc.want)
			}

			res := runModification(context.Background(), files, rootDir, t.TempDir(), params, nil, nil)
			if res.Failed != 0 {
				t.Fatalf("failures: %v", res.Failures)
			}
			if got := res.PHIRisks[phiRiskText]; got != len(tc.want) {
				t.Errorf("run counted %d files shipped with text, want %d", got, len(tc.want))
			}
		})
	}
}
