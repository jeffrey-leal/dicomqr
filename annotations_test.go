package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"fyne.io/fyne/v2/test"
	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/tag"
)

const sopGSPS = "1.2.840.10008.5.1.4.1.1.11.1"

// gspsAnnotationElements builds the annotation content of a presentation
// state: three annotations (text and shape, text alone, shape alone), a layer
// with a typed description, and a group whose label a reviewer typed, plus an
// Unformatted Text Value nested somewhere no annotation lives, to prove the
// modes reach every depth.
func gspsAnnotationElements(t *testing.T) []*sdicom.Element {
	t.Helper()
	text := func(s string) *sdicom.Element {
		return mustTestElement(t, tag.TextObjectSequence, [][]*sdicom.Element{{
			mustTestElement(t, tag.AnchorPointAnnotationUnits, []string{"PIXEL"}),
			mustTestElement(t, tag.UnformattedTextValue, []string{s}),
			mustTestElement(t, tag.AnchorPoint, []float64{10, 12}),
			mustTestElement(t, tag.AnchorPointVisibility, []string{"Y"}),
			mustTestElement(t, tag.GraphicGroupID, []int{7}),
		}})
	}
	shape := func() *sdicom.Element {
		return mustTestElement(t, tag.GraphicObjectSequence, [][]*sdicom.Element{{
			mustTestElement(t, tag.GraphicAnnotationUnits, []string{"PIXEL"}),
			mustTestElement(t, tag.GraphicDimensions, []int{2}),
			mustTestElement(t, tag.NumberOfGraphicPoints, []int{2}),
			mustTestElement(t, tag.GraphicData, []float64{1, 2, 30, 40}),
			mustTestElement(t, tag.GraphicType, []string{"POLYLINE"}),
			mustTestElement(t, tag.GraphicGroupID, []int{7}),
		}})
	}
	layer := func() *sdicom.Element { return mustTestElement(t, tag.GraphicLayer, []string{"LAYER1"}) }
	return []*sdicom.Element{
		mustTestElement(t, tag.RequestAttributesSequence, [][]*sdicom.Element{{
			mustTestElement(t, tag.UnformattedTextValue, []string{"stray J. DOE"}),
		}}),
		mustTestElement(t, tag.GraphicAnnotationSequence, [][]*sdicom.Element{
			{layer(), text("J. DOE 12.3 mm"), shape()},
			{layer(), text("call Dr Smith")},
			{layer(), shape()},
		}),
		mustTestElement(t, tag.GraphicLayerSequence, [][]*sdicom.Element{{
			mustTestElement(t, tag.GraphicLayer, []string{"LAYER1"}),
			mustTestElement(t, tag.GraphicLayerDescription, []string{"Dr Smith's notes"}),
		}}),
		mustTestElement(t, tag.ContentLabel, []string{"GSPS"}),
		mustTestElement(t, tag.GraphicGroupSequence, [][]*sdicom.Element{{
			mustTestElement(t, tag.GraphicGroupLabel, []string{"DOE LESION"}),
			mustTestElement(t, tag.GraphicGroupDescription, []string{"seen by J. Doe"}),
			mustTestElement(t, tag.GraphicGroupID, []int{7}),
		}}),
	}
}

// countNested counts elements with tag t at any depth.
func countNested(elems []*sdicom.Element, t tag.Tag) int {
	n := 0
	for _, e := range elems {
		if e == nil {
			continue
		}
		if e.Tag == t {
			n++
		}
		if e.Value != nil && e.Value.ValueType() == sdicom.Sequences {
			items, _ := e.Value.GetValue().([]*sdicom.SequenceItemValue)
			for _, item := range items {
				sub, _ := item.GetValue().([]*sdicom.Element)
				n += countNested(sub, t)
			}
		}
	}
	return n
}

// topSequenceItems returns the items of the top-level sequence t, or nil.
func topSequenceItems(elems []*sdicom.Element, t tag.Tag) [][]*sdicom.Element {
	for _, e := range elems {
		if e.Tag != t {
			continue
		}
		items, _ := e.Value.GetValue().([]*sdicom.SequenceItemValue)
		out := make([][]*sdicom.Element, 0, len(items))
		for _, item := range items {
			sub, _ := item.GetValue().([]*sdicom.Element)
			out = append(out, sub)
		}
		return out
	}
	return nil
}

// stringsOf returns every string value of tag t at any depth.
func stringsOf(elems []*sdicom.Element, t tag.Tag) []string {
	var out []string
	for _, e := range elems {
		if e.Tag == t {
			if s, ok := e.Value.GetValue().([]string); ok {
				out = append(out, s...)
			}
		}
		if e.Value != nil && e.Value.ValueType() == sdicom.Sequences {
			items, _ := e.Value.GetValue().([]*sdicom.SequenceItemValue)
			for _, item := range items {
				sub, _ := item.GetValue().([]*sdicom.Element)
				out = append(out, stringsOf(sub, t)...)
			}
		}
	}
	return out
}

func TestAnnotationModeTokens(t *testing.T) {
	for _, tc := range []struct {
		in   string
		mode string
		ok   bool
	}{
		{"", annotationsKeep, true},
		{"keep", annotationsKeep, true},
		{" KEEP ", annotationsKeep, true},
		{"notext", annotationsNoText, true},
		{"NoText", annotationsNoText, true},
		{" remove", annotationsRemove, true},
		{"strip", "", false},
	} {
		mode, ok := annotationMode(tc.in)
		if mode != tc.mode || ok != tc.ok {
			t.Errorf("annotationMode(%q) = %q, %v; want %q, %v", tc.in, mode, ok, tc.mode, tc.ok)
		}
	}
	for _, token := range []string{annotationsKeep, annotationsNoText, annotationsRemove} {
		if got := annotationsFromLabel(annotationsLabelFor(token)); got != token {
			t.Errorf("label round trip of %q = %q", token, got)
		}
	}
	// An unrecognised token shows as the strictest choice, so saving it from
	// the editor can only tighten what was written.
	if got := annotationsLabelFor("strip"); got != annotationsLabelRemove {
		t.Errorf("unrecognised token shows as %q, want Remove", got)
	}
}

// Keep shapes, remove text: every text box goes and the shapes stay; an
// annotation that was only text is dropped rather than left empty; the layer
// and group descriptions go, and the required group label is neutralised.
func TestApplyAnnotationModeNoText(t *testing.T) {
	out := applyAnnotationMode(gspsAnnotationElements(t), annotationsNoText)

	if n := countNested(out, tag.UnformattedTextValue); n != 0 {
		t.Errorf("%d Unformatted Text Values survived", n)
	}
	if n := countNested(out, tag.TextObjectSequence); n != 0 {
		t.Errorf("%d Text Object Sequences survived", n)
	}
	items := topSequenceItems(out, tag.GraphicAnnotationSequence)
	if len(items) != 2 {
		t.Fatalf("annotation items = %d, want 2 (the text-only one dropped)", len(items))
	}
	for i, item := range items {
		if !hasSequenceItems(item, tag.GraphicObjectSequence) {
			t.Errorf("annotation %d lost its shape", i)
		}
	}
	if n := countNested(out, tag.GraphicData); n != 2 {
		t.Errorf("Graphic Data count = %d, want both shapes kept", n)
	}
	if n := countNested(out, tag.GraphicLayerDescription); n != 0 {
		t.Errorf("layer description survived")
	}
	if got := stringsOf(out, tag.GraphicLayer); len(got) != 3 {
		t.Errorf("graphic layers = %v, want the layer definition and both kept annotations' layers", got)
	}
	if n := countNested(out, tag.GraphicGroupDescription); n != 0 {
		t.Errorf("group description survived")
	}
	if got := stringsOf(out, tag.GraphicGroupLabel); !reflect.DeepEqual(got, []string{"GROUP 7"}) {
		t.Errorf("group label = %v, want it neutralised from the group's ID", got)
	}
	if got := stringsOf(out, tag.ContentLabel); !reflect.DeepEqual(got, []string{"GSPS"}) {
		t.Errorf("Content Label = %v — identification is the tag rules' business", got)
	}
}

// With only text annotations, notext leaves no annotation at all, and the
// sequence goes rather than stay empty.
func TestApplyAnnotationModeNoTextDropsEmptySequence(t *testing.T) {
	elems := []*sdicom.Element{
		mustTestElement(t, tag.GraphicAnnotationSequence, [][]*sdicom.Element{{
			mustTestElement(t, tag.GraphicLayer, []string{"LAYER1"}),
			mustTestElement(t, tag.TextObjectSequence, [][]*sdicom.Element{{
				mustTestElement(t, tag.UnformattedTextValue, []string{"J. DOE"}),
			}}),
		}}),
		mustTestElement(t, tag.ContentLabel, []string{"GSPS"}),
	}
	out := applyAnnotationMode(elems, annotationsNoText)
	if countNested(out, tag.GraphicAnnotationSequence) != 0 {
		t.Errorf("an empty Graphic Annotation Sequence was kept")
	}
	if countNested(out, tag.ContentLabel) != 1 {
		t.Errorf("Content Label lost")
	}
}

// Remove: every shape and every piece of text goes; the layer list stays
// (minus its description) and so does everything else.
func TestApplyAnnotationModeRemove(t *testing.T) {
	out := applyAnnotationMode(gspsAnnotationElements(t), annotationsRemove)
	for _, gone := range []tag.Tag{tag.GraphicAnnotationSequence, tag.GraphicObjectSequence, tag.TextObjectSequence,
		tag.UnformattedTextValue, tag.GraphicGroupSequence, tag.GraphicLayerDescription, tag.GraphicData} {
		if n := countNested(out, gone); n != 0 {
			t.Errorf("%s survived Remove (%d)", gone, n)
		}
	}
	if countNested(out, tag.GraphicLayerSequence) != 1 || countNested(out, tag.ContentLabel) != 1 {
		t.Errorf("Remove took more than the annotations")
	}
}

func TestApplyAnnotationModeKeep(t *testing.T) {
	in := gspsAnnotationElements(t)
	want := countNested(in, tag.UnformattedTextValue)
	out := applyAnnotationMode(in, annotationsKeep)
	if got := countNested(out, tag.UnformattedTextValue); got != want || len(out) != len(in) {
		t.Errorf("Keep changed the dataset")
	}
}

func TestCompileAnnotations(t *testing.T) {
	if _, err := compileModifyParams(ModProfile{Annotations: "strip"}); err == nil ||
		!strings.Contains(err.Error(), "annotations") {
		t.Errorf("an unrecognised annotations value compiled: %v", err)
	}
	p, err := compileModifyParams(ModProfile{Annotations: "NoText"})
	if err != nil {
		t.Fatalf("annotations alone is an action: %v", err)
	}
	if p.annotations != annotationsNoText {
		t.Errorf("compiled mode = %q", p.annotations)
	}
	if _, err := compileModifyParams(ModProfile{Annotations: "keep"}); err == nil {
		t.Errorf("keep alone compiled — it does nothing, so it is not an action")
	}
}

// A presentation state run through the real engine: what each mode exports,
// written and parsed back — the rebuilt sequences and the neutralised label
// must survive the writer.
func TestRunModificationAnnotations(t *testing.T) {
	rootDir := t.TempDir()
	src := filepath.Join(rootDir, "pr.dcm")
	writePHIFixture(t, src, sopGSPS, "PR", "", gspsAnnotationElements(t)...)

	for _, tc := range []struct {
		mode               string
		texts, shapes      int
		annotationsPresent bool
	}{
		{annotationsKeep, 3, 2, true},
		{annotationsNoText, 0, 2, true},
		{annotationsRemove, 0, 0, false},
	} {
		t.Run("mode="+tc.mode, func(t *testing.T) {
			params, err := compileModifyParams(ModProfile{Sets: []string{"0010,0010=ANON"}, Annotations: tc.mode})
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			outDir := t.TempDir()
			res := runModification(context.Background(), []string{src}, rootDir, outDir, params, nil, nil)
			if res.Processed != 1 || res.Failed != 0 {
				t.Fatalf("result = %+v (%v), want the file exported", res, res.Failures)
			}
			out, err := sdicom.ParseFile(filepath.Join(outDir, "pr.dcm"), nil)
			if err != nil {
				t.Fatalf("parse export: %v", err)
			}
			if n := countNested(out.Elements, tag.UnformattedTextValue); n != tc.texts {
				t.Errorf("text values = %d, want %d", n, tc.texts)
			}
			if n := countNested(out.Elements, tag.GraphicData); n != tc.shapes {
				t.Errorf("shapes = %d, want %d", n, tc.shapes)
			}
			if got := countNested(out.Elements, tag.GraphicAnnotationSequence) == 1; got != tc.annotationsPresent {
				t.Errorf("annotation sequence present = %v, want %v", got, tc.annotationsPresent)
			}
			if got := stringsOf(out.Elements, tag.PatientName); !reflect.DeepEqual(got, []string{"ANON"}) {
				t.Errorf("Patient Name = %v — the rest of the profile must still run", got)
			}
		})
	}
}

// The PHI check and the run agree on annotation text, as they do on every
// other text field: kept text is flagged and counted, and either mode — or a
// Remove tags list that takes the annotations — clears both.
func TestPHIAnnotationTextScreenMatchesRun(t *testing.T) {
	rootDir := t.TempDir()
	src := filepath.Join(rootDir, "pr.dcm")
	writePHIFixture(t, src, sopGSPS, "PR", "", gspsAnnotationElements(t)...)
	files := []string{src}

	for _, tc := range []struct {
		name    string
		profile ModProfile
		want    int
	}{
		{"kept", ModProfile{Sets: []string{"0010,0010=ANON"}}, 1},
		{"shapes only", ModProfile{Annotations: annotationsNoText}, 0},
		{"removed", ModProfile{Annotations: annotationsRemove}, 0},
		{"by tag rules", ModProfile{Removes: []string{"0070,0001", "0040,0275"}}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params, err := compileModifyParams(tc.profile)
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			screened, _ := screenPHIFiles(files, params, nil, nil)
			f := evaluatePHIScreen(screened, func(m string) phiRules {
				return phiRules{textHandled: textRulesFor(params, m)}
			})
			if got := len(f.byRisk[phiRiskText]); got != tc.want {
				t.Errorf("screen flagged %d files for text, want %d (%s)", got, tc.want, f.textBreakdown())
			}
			res := runModification(context.Background(), files, rootDir, t.TempDir(), params, nil, nil)
			if res.Failed != 0 {
				t.Fatalf("failures: %v", res.Failures)
			}
			if got := res.PHIRisks[phiRiskText]; got != tc.want {
				t.Errorf("run counted %d files shipped with text, want %d", got, tc.want)
			}
		})
	}
}

func TestModProfileAnnotationAdvisory(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    ModProfile
		want []string // substrings; nil = no advisory
	}{
		{"keep, nothing in the way", ModProfile{Removes: []string{"0008,0080"}}, nil},
		{"keep, annotations removed by tag", ModProfile{Removes: []string{"0070,0001", "0070,0006", "0008,0080"}},
			[]string{"0070,0001 and 0070,0006", "the annotations", "them"}},
		{"shapes only, text removed by tag is no conflict", ModProfile{Annotations: annotationsNoText,
			Removes: []string{"70,6", "0070,0008"}}, nil},
		{"shapes only, shapes removed by tag", ModProfile{Annotations: annotationsNoText,
			Removes: []string{"0070,0009"}}, []string{"0070,0009", "the shapes", "it out"}},
		{"keep, PR skipped", ModProfile{IgnoreModalities: []string{"SR", " pr "}}, []string{"Ignore modalities skips PR"}},
		{"remove has nothing to undo", ModProfile{Annotations: annotationsRemove,
			Removes: []string{"0070,0001"}, IgnoreModalities: []string{"PR"}}, nil},
		{"unrecognised value is compile's to report", ModProfile{Annotations: "strip",
			Removes: []string{"0070,0001"}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := modProfileAnnotationAdvisory(tc.p)
			if tc.want == nil {
				if got != "" {
					t.Errorf("advisory = %q, want none", got)
				}
				return
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("advisory %q lacks %q", got, w)
				}
			}
		})
	}
}

// The shipped profiles state the annotation choice through the setting rather
// than through Remove tags, so switching it to Keep in a copy actually keeps
// them, and they raise no advisory.
func TestEmbeddedDefaultsAnnotations(t *testing.T) {
	var cfg ModProfileConfig
	if err := json.Unmarshal(defaultModProfilesJSON, &cfg); err != nil {
		t.Fatal(err)
	}
	for name, p := range cfg {
		if p.Annotations != annotationsRemove {
			t.Errorf("%s: annotations = %q, want remove", name, p.Annotations)
		}
		if adv := modProfileAnnotationAdvisory(p); adv != "" {
			t.Errorf("%s raises an annotations advisory: %s", name, adv)
		}
		keep := p
		keep.Annotations = annotationsNoText
		keep.IgnoreModalities = nil
		if adv := modProfileAnnotationAdvisory(keep); adv != "" {
			t.Errorf("%s: switching to shapes-only would still be undone by its tag rules: %s", name, adv)
		}
		keep.Annotations = annotationsKeep
		if adv := modProfileAnnotationAdvisory(keep); adv != "" {
			t.Errorf("%s: switching to keep would still be undone by its tag rules: %s", name, adv)
		}
	}
}

func TestNormalizeAnnotationsToken(t *testing.T) {
	for in, want := range map[string]string{"KEEP": "", "NoText": "notext", "remove": "remove", "strip": "strip"} {
		if got := normalizeModProfile(ModProfile{Annotations: in}).Annotations; got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

// The editor saves the choice, reads an unrecognised value as Remove, and
// computes its advisory from the live controls.
func TestModProfileEditorAnnotations(t *testing.T) {
	test.NewApp()
	ed := newModProfileEditor("p", ModProfile{Annotations: annotationsNoText, Priv: true}, ModProfileConfig{})
	if _, saved, err := ed.validate(); err != nil || saved.Annotations != annotationsNoText {
		t.Errorf("saved annotations = %q (%v)", saved.Annotations, err)
	}
	ed.annotationsSel.SetSelected(annotationsLabelKeep)
	if _, saved, _ := ed.validate(); saved.Annotations != "" {
		t.Errorf("Keep saved as %q, want the empty default", saved.Annotations)
	}

	ed = newModProfileEditor("p", ModProfile{Annotations: "strip", Priv: true}, ModProfileConfig{})
	if _, saved, _ := ed.validate(); saved.Annotations != annotationsRemove {
		t.Errorf("unrecognised value saved as %q, want remove", saved.Annotations)
	}

	ed = newModProfileEditor("p", ModProfile{Priv: true}, ModProfileConfig{})
	if adv := ed.annotationAdvisory(); adv != "" {
		t.Errorf("advisory with nothing in the way: %q", adv)
	}
	ed.fields.removes.SetText("0070,0001  Graphic Annotation Sequence\n0008,0080")
	if adv := ed.annotationAdvisory(); !strings.Contains(adv, "0070,0001") {
		t.Errorf("advisory = %q, want it to name 0070,0001", adv)
	}
	ed.annotationsSel.SetSelected(annotationsLabelRemove)
	if adv := ed.annotationAdvisory(); adv != "" {
		t.Errorf("advisory under Remove: %q", adv)
	}
	ed.annotationsSel.SetSelected(annotationsLabelKeep)
	ed.fields.removes.SetText("")
	ed.ignoreModsEntry.SetText("SR, PR")
	if adv := ed.annotationAdvisory(); !strings.Contains(adv, "skips PR") {
		t.Errorf("advisory = %q, want it to say PR is skipped", adv)
	}
}

// The writer must accept the neutralised group label as written — guard
// against NewElement ever choosing a VR the dictionary does not list.
func TestNeutralisedGroupLabelWrites(t *testing.T) {
	item := []*sdicom.Element{
		mustTestElement(t, tag.GraphicGroupID, []int{3}),
		mustTestElement(t, tag.GraphicGroupLabel, []string{"DOE"}),
	}
	neutraliseGroupLabel(item, 1)
	ds := sdicom.Dataset{Elements: []*sdicom.Element{
		mustTestElement(t, tag.MediaStorageSOPClassUID, []string{sopGSPS}),
		mustTestElement(t, tag.MediaStorageSOPInstanceUID, []string{"1.2.3"}),
		mustTestElement(t, tag.TransferSyntaxUID, []string{tsExplicitVRLE}),
		mustTestElement(t, tag.GraphicGroupSequence, [][]*sdicom.Element{item}),
	}}
	f, err := os.Create(filepath.Join(t.TempDir(), "g.dcm"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := sdicom.Write(f, ds); err != nil {
		t.Fatalf("write with full verification: %v", err)
	}
	if got := stringsOf(ds.Elements, tag.GraphicGroupLabel); !reflect.DeepEqual(got, []string{"GROUP 3"}) {
		t.Errorf("label = %v", got)
	}
}
