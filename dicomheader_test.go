package main

import (
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/frame"
	"github.com/suyashkumar/dicom/pkg/tag"
	"github.com/suyashkumar/dicom/pkg/uid"
)

// writeHeaderFixture writes a file whose header reaches well past group 0020 —
// a nested sequence in group 0054, which is where the viewer's chapter labels
// read from — followed by pixelBytes of native pixel data and, when trailing is
// set, an element after the pixel data.
func writeHeaderFixture(t *testing.T, path, ts string, pixelBytes int, trailing bool) {
	t.Helper()
	cols := 1024
	rows := max(1, pixelBytes/cols)
	nf := frame.NewNativeFrame[uint8](8, rows, cols, rows*cols, 1)
	pd, err := sdicom.NewElement(tag.PixelData, sdicom.PixelDataInfo{
		Frames: []*frame.Frame{{Encapsulated: false, NativeData: nf}},
	})
	if err != nil {
		t.Fatalf("NewElement(PixelData): %v", err)
	}
	view, err := sdicom.NewElement(tag.ViewCodeSequence, [][]*sdicom.Element{{
		mustTestElement(t, tag.CodeMeaning, []string{"Apical 4 chamber"}),
	}})
	if err != nil {
		t.Fatalf("NewElement(ViewCodeSequence): %v", err)
	}
	elems := []*sdicom.Element{
		mustTestElement(t, tag.MediaStorageSOPClassUID, []string{"1.2.840.10008.5.1.4.1.1.7"}),
		mustTestElement(t, tag.MediaStorageSOPInstanceUID, []string{"1.2.3.4.5"}),
		mustTestElement(t, tag.TransferSyntaxUID, []string{ts}),
		mustTestElement(t, tag.SOPInstanceUID, []string{"1.2.3.4.5"}),
		mustTestElement(t, tag.Modality, []string{"US"}),
		mustTestElement(t, tag.PatientName, []string{"DOE^JANE"}),
		mustTestElement(t, tag.SeriesInstanceUID, []string{"1.2.3.4.1"}),
		mustTestElement(t, tag.InstanceNumber, []string{"7"}),
		mustTestElement(t, tag.SamplesPerPixel, []int{1}),
		mustTestElement(t, tag.PhotometricInterpretation, []string{"MONOCHROME2"}),
		mustTestElement(t, tag.Rows, []int{rows}),
		mustTestElement(t, tag.Columns, []int{cols}),
		mustTestElement(t, tag.BitsAllocated, []int{8}),
		mustTestElement(t, tag.BitsStored, []int{8}),
		mustTestElement(t, tag.HighBit, []int{7}),
		mustTestElement(t, tag.PixelRepresentation, []int{0}),
		view,
		pd,
	}
	if trailing {
		elems = append(elems, mustTestElement(t, tag.DataSetTrailingPadding, []byte{0, 0}))
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	if err := sdicom.Write(f, sdicom.Dataset{Elements: elems},
		sdicom.SkipVRVerification(), sdicom.SkipValueTypeVerification()); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}

// countingReader counts the bytes actually pulled from the file.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// The header read must return exactly what the SkipPixelData parse it replaces
// returned, less the pixel data and whatever follows it — in every byte order
// the peek has to decode, since a peek read in the wrong order would stop at
// the wrong place or never stop at all.
func TestReadDicomHeaderMatchesSkipPixelDataParse(t *testing.T) {
	for _, ts := range []string{uid.ExplicitVRLittleEndian, uid.ImplicitVRLittleEndian, uid.ExplicitVRBigEndian} {
		t.Run(transferSyntaxLabel(ts), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "f.dcm")
			writeHeaderFixture(t, path, ts, 64<<10, true)

			full, err := safeParseFile(path, nil, sdicom.SkipPixelData())
			if err != nil {
				t.Fatalf("reference parse: %v", err)
			}
			got, err := readDicomHeader(path)
			if err != nil {
				t.Fatalf("readDicomHeader: %v", err)
			}

			var want []*sdicom.Element
			for _, e := range full.Elements {
				if e.Tag.Group >= tag.PixelData.Group {
					break
				}
				want = append(want, e)
			}
			if len(got.Elements) != len(want) {
				t.Fatalf("header holds %d elements, want %d (every element before the pixel data)",
					len(got.Elements), len(want))
			}
			for i := range want {
				if got.Elements[i].Tag != want[i].Tag || got.Elements[i].Value.String() != want[i].Value.String() {
					t.Errorf("element %d = %v %s, want %v %s", i,
						got.Elements[i].Tag, got.Elements[i].Value, want[i].Tag, want[i].Value)
				}
			}
			if _, err := got.FindElementByTag(tag.PixelData); err == nil {
				t.Error("header contains the pixel data element")
			}
			if c := codeMeaning(&got, tag.ViewCodeSequence); c != "Apical 4 chamber" {
				t.Errorf("nested sequence value = %q, want it read intact", c)
			}
		})
	}
}

// The point of the reader: it stops before the pixel data rather than reading
// it to throw away, so the bytes it pulls are bounded by the header plus one
// read-ahead buffer, however large the image.
func TestReadDicomHeaderDoesNotReadPixelData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.dcm")
	const pixelBytes = 4 << 20
	writeHeaderFixture(t, path, uid.ExplicitVRLittleEndian, pixelBytes, false)

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, _ := f.Stat()
	cr := &countingReader{r: f}
	ds, err := readDicomHeaderFrom(cr, info.Size(), path)
	if err != nil {
		t.Fatalf("readDicomHeaderFrom: %v", err)
	}
	if datasetInt(&ds, tag.Rows, 0) == 0 {
		t.Fatal("header read lost Rows")
	}
	if limit := int64(2 * headerReadBufferSize); cr.n > limit {
		t.Errorf("read %d of %d bytes; a header read should stop within %d", cr.n, info.Size(), limit)
	}
}

// A Deflated transfer syntax puts compressed bytes in the buffer the peek reads,
// so the reader must not trust it — and a file with no Transfer Syntax UID has a
// byte order the library infers rather than states. Both fall back to reading on.
func TestHeaderPeekByteOrderDistrustsDeflateAndMissingSyntax(t *testing.T) {
	for _, tc := range []struct {
		ts       string
		peekable bool
	}{
		{uid.ExplicitVRLittleEndian, true},
		{uid.ImplicitVRLittleEndian, true},
		{uid.ExplicitVRBigEndian, true},
		{tsJPEG2000LL, true},
		{uid.DeflatedExplicitVRLittleEndian, false},
		{"", false},
		{"1.2.3.not.a.syntax", false},
	} {
		var meta sdicom.Dataset
		if tc.ts != "" {
			meta.Elements = []*sdicom.Element{mustTestElement(t, tag.TransferSyntaxUID, []string{tc.ts})}
		}
		if _, ok := headerPeekByteOrder(&meta); ok != tc.peekable {
			t.Errorf("syntax %q: peekable = %v, want %v", tc.ts, ok, tc.peekable)
		}
	}
}

// sortElementsByTag puts a fixture's top-level elements back into ascending tag
// order after a test has appended one. The standard requires that order, and the
// header reader relies on it: it stops at the pixel data, so an element appended
// after it (which the library's writer emits exactly where it is placed) is
// beyond where a header read looks.
func sortElementsByTag(ds *sdicom.Dataset) {
	sort.SliceStable(ds.Elements, func(i, j int) bool {
		a, b := ds.Elements[i].Tag, ds.Elements[j].Tag
		if a.Group != b.Group {
			return a.Group < b.Group
		}
		return a.Element < b.Element
	})
}
