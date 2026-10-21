package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/frame"
	"github.com/suyashkumar/dicom/pkg/tag"
	"github.com/suyashkumar/dicom/pkg/uid"
)

// nativeFixture describes an uncompressed test file.
type nativeFixture struct {
	ts                         string
	rows, cols, spp, bits      int
	frames                     int
	declaredFrames             string // NumberOfFrames as written; "" = from frames, omitted for one
	signed                     bool
	photometric                string
	slope, intercept           string
	withOverlay, withWindowing bool
}

// writeNativeFixture writes fx to path, every sample distinct enough that a
// frame read from the wrong offset, or in the wrong byte order, cannot match.
func writeNativeFixture(t *testing.T, path string, fx nativeFixture) {
	t.Helper()
	pixels := fx.rows * fx.cols
	var frames []*frame.Frame
	for f := 0; f < fx.frames; f++ {
		switch fx.bits {
		case 8:
			nf := frame.NewNativeFrame[uint8](8, fx.rows, fx.cols, pixels, fx.spp)
			for k := range nf.RawData {
				nf.RawData[k] = uint8(k*7 + f*31 + 3)
			}
			frames = append(frames, &frame.Frame{NativeData: nf})
		case 16:
			nf := frame.NewNativeFrame[uint16](16, fx.rows, fx.cols, pixels, fx.spp)
			for k := range nf.RawData {
				// High bytes vary too, and the top bit is set for some samples,
				// so a signed reading has negatives.
				nf.RawData[k] = uint16(k*977 + f*7919 + 0x0102)
			}
			frames = append(frames, &frame.Frame{NativeData: nf})
		case 32:
			nf := frame.NewNativeFrame[uint32](32, fx.rows, fx.cols, pixels, fx.spp)
			for k := range nf.RawData {
				nf.RawData[k] = uint32(k + f)
			}
			frames = append(frames, &frame.Frame{NativeData: nf})
		}
	}
	pd, err := sdicom.NewElement(tag.PixelData, sdicom.PixelDataInfo{Frames: frames})
	if err != nil {
		t.Fatalf("NewElement(PixelData): %v", err)
	}
	pixelRep := 0
	if fx.signed {
		pixelRep = 1
	}
	photometric := fx.photometric
	if photometric == "" {
		photometric = "MONOCHROME2"
		if fx.spp == 3 {
			photometric = "RGB"
		}
	}
	elems := []*sdicom.Element{
		mustTestElement(t, tag.MediaStorageSOPClassUID, []string{"1.2.840.10008.5.1.4.1.1.6.1"}),
		mustTestElement(t, tag.MediaStorageSOPInstanceUID, []string{"1.2.3.9"}),
		mustTestElement(t, tag.TransferSyntaxUID, []string{fx.ts}),
		mustTestElement(t, tag.SOPClassUID, []string{"1.2.840.10008.5.1.4.1.1.6.1"}),
		mustTestElement(t, tag.SOPInstanceUID, []string{"1.2.3.9"}),
		mustTestElement(t, tag.Modality, []string{"US"}),
		mustTestElement(t, tag.PatientName, []string{"DOE^JANE"}),
		mustTestElement(t, tag.SamplesPerPixel, []int{fx.spp}),
		mustTestElement(t, tag.PhotometricInterpretation, []string{photometric}),
		mustTestElement(t, tag.Rows, []int{fx.rows}),
		mustTestElement(t, tag.Columns, []int{fx.cols}),
		mustTestElement(t, tag.BitsAllocated, []int{fx.bits}),
		mustTestElement(t, tag.BitsStored, []int{fx.bits}),
		mustTestElement(t, tag.HighBit, []int{fx.bits - 1}),
		mustTestElement(t, tag.PixelRepresentation, []int{pixelRep}),
		pd,
	}
	if fx.spp == 3 {
		elems = append(elems, mustTestElement(t, tag.PlanarConfiguration, []int{0}))
	}
	declared := fx.declaredFrames
	if declared == "" && fx.frames > 1 {
		declared = strconv.Itoa(fx.frames)
	}
	if declared != "" {
		elems = append(elems, mustTestElement(t, tag.NumberOfFrames, []string{declared}))
	}
	if fx.slope != "" {
		elems = append(elems,
			mustTestElement(t, tag.RescaleSlope, []string{fx.slope}),
			mustTestElement(t, tag.RescaleIntercept, []string{fx.intercept}))
	}
	if fx.withWindowing {
		elems = append(elems,
			mustTestElement(t, tag.WindowCenter, []string{"40"}),
			mustTestElement(t, tag.WindowWidth, []string{"400"}))
	}
	if fx.withOverlay {
		// Overlay planes precede the pixel data, so they sit between the header
		// read's stopping point and everything before it — the offset must
		// account for them.
		elems = append(elems,
			mustTestElement(t, tag.Tag{Group: 0x6000, Element: 0x0010}, []int{fx.rows}),
			mustTestElement(t, tag.Tag{Group: 0x6000, Element: 0x0011}, []int{fx.cols}),
			mustTestElement(t, tag.Tag{Group: 0x6000, Element: 0x0040}, []string{"G"}),
			mustTestElement(t, tag.Tag{Group: 0x6000, Element: 0x0050}, []int{1, 1}),
			mustTestElement(t, tag.Tag{Group: 0x6000, Element: 0x0100}, []int{1}),
			mustTestElement(t, tag.Tag{Group: 0x6000, Element: 0x0102}, []int{0}),
			mustTestElement(t, tag.Tag{Group: 0x6000, Element: 0x3000}, make([]byte, (fx.rows*fx.cols+15)/16*2)))
	}
	ds := sdicom.Dataset{Elements: elems}
	sortElementsByTag(&ds)
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	if err := sdicom.Write(f, ds, sdicom.SkipVRVerification(), sdicom.SkipValueTypeVerification()); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// The frame-by-frame path must be indistinguishable from the full parse it
// replaces: the same decode parameters, annotations and overlays, and every
// frame holding the same samples — so every decode downstream is too.
func TestNativeFramesMatchFullParse(t *testing.T) {
	cases := map[string]nativeFixture{
		"8-bit grey, odd length":     {rows: 3, cols: 3, spp: 1, bits: 8, frames: 1},
		"8-bit RGB clip":             {rows: 6, cols: 5, spp: 3, bits: 8, frames: 4, withOverlay: true},
		"16-bit grey stack":          {rows: 4, cols: 7, spp: 1, bits: 16, frames: 3, withWindowing: true},
		"16-bit signed, rescaled":    {rows: 5, cols: 5, spp: 1, bits: 16, frames: 2, signed: true, slope: "2", intercept: "-1024"},
		"16-bit MONOCHROME1, single": {rows: 8, cols: 4, spp: 1, bits: 16, frames: 1, photometric: "MONOCHROME1"},
	}
	for _, ts := range []string{uid.ExplicitVRLittleEndian, uid.ImplicitVRLittleEndian} {
		for name, fx := range cases {
			t.Run(transferSyntaxLabel(ts)+"/"+name, func(t *testing.T) {
				fx.ts = ts
				path := filepath.Join(t.TempDir(), "f.dcm")
				writeNativeFixture(t, path, fx)

				fast, err := parseDicomFile(path)
				if err != nil {
					t.Fatalf("parseDicomFile: %v", err)
				}
				if fast.native == nil {
					t.Fatal("the frame-by-frame path was not taken")
				}
				full, err := parseDicomFileFull(path)
				if err != nil {
					t.Fatalf("full parse: %v", err)
				}

				fp, ff := *fast, *full
				fp.native, ff.frames = nil, nil
				if !reflect.DeepEqual(fp, ff) {
					t.Errorf("parameters differ:\n fast %+v\n full %+v", fp, ff)
				}
				if fast.frameCount() != full.frameCount() || fast.frameCount() != fx.frames {
					t.Fatalf("frame count %d, full parse %d, written %d", fast.frameCount(), full.frameCount(), fx.frames)
				}
				for i := 0; i < fx.frames; i++ {
					a, err := fast.frameAt(i)
					if err != nil {
						t.Fatalf("frame %d: %v", i, err)
					}
					b, _ := full.frameAt(i)
					na, _ := a.GetNativeFrame()
					nb, _ := b.GetNativeFrame()
					if na.Rows() != nb.Rows() || na.Cols() != nb.Cols() ||
						na.SamplesPerPixel() != nb.SamplesPerPixel() || na.BitsPerSample() != nb.BitsPerSample() {
						t.Errorf("frame %d geometry differs", i)
					}
					if !reflect.DeepEqual(na.RawDataSlice(), nb.RawDataSlice()) {
						t.Errorf("frame %d samples differ", i)
					}
					sa, err := fast.frameState(i)
					if err != nil {
						t.Fatalf("decode %d: %v", i, err)
					}
					sb, _ := full.frameState(i)
					if !reflect.DeepEqual(sa.img, sb.img) || sa.label != sb.label {
						t.Errorf("frame %d renders differently", i)
					}
				}
			})
		}
	}
}

// Everything the library would read differently, or refuse, goes through the
// full parse: the result — image or error — is what it always was.
func TestNativeFramesFallBack(t *testing.T) {
	dir := t.TempDir()

	enc := filepath.Join(dir, "jpeg.dcm")
	writeEncapsulatedTestDICOM(t, enc, tsJPEGBaseline)

	be := filepath.Join(dir, "be.dcm")
	writeNativeFixture(t, be, nativeFixture{ts: uid.ExplicitVRBigEndian, rows: 4, cols: 4, spp: 1, bits: 16, frames: 2})

	wide := filepath.Join(dir, "32.dcm")
	writeNativeFixture(t, wide, nativeFixture{ts: uid.ExplicitVRLittleEndian, rows: 4, cols: 4, spp: 1, bits: 32, frames: 1})

	mismatch := filepath.Join(dir, "mismatch.dcm")
	writeNativeFixture(t, mismatch, nativeFixture{ts: uid.ExplicitVRLittleEndian, rows: 4, cols: 4, spp: 1, bits: 16,
		frames: 2, declaredFrames: "3"})

	truncated := filepath.Join(dir, "truncated.dcm")
	writeNativeFixture(t, truncated, nativeFixture{ts: uid.ImplicitVRLittleEndian, rows: 4, cols: 4, spp: 1, bits: 16, frames: 3})
	if st, err := os.Stat(truncated); err == nil {
		if err := os.Truncate(truncated, st.Size()-10); err != nil {
			t.Fatal(err)
		}
	}

	for name, path := range map[string]string{"encapsulated": enc, "big endian": be, "32-bit": wide,
		"length mismatch": mismatch, "truncated": truncated} {
		t.Run(name, func(t *testing.T) {
			if _, _, ok := openNativeFrameSource(path); ok {
				t.Fatal("the frame-by-frame path accepted a file it must leave to the full parse")
			}
			p, err := parseDicomFile(path)
			_, fullErr := parseDicomFileFull(path)
			if (err == nil) != (fullErr == nil) {
				t.Fatalf("parseDicomFile err = %v, full parse err = %v", err, fullErr)
			}
			if err == nil && p.native != nil {
				t.Errorf("parsed through the frame source")
			}
		})
	}
}

// A frame is read when it is asked for, from where it sits — not at open.
func TestNativeFramesReadOnDemand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clip.dcm")
	writeNativeFixture(t, path, nativeFixture{ts: uid.ExplicitVRLittleEndian, rows: 4, cols: 4, spp: 1, bits: 8, frames: 4})
	_, src, ok := openNativeFrameSource(path)
	if !ok {
		t.Fatal("not opened")
	}
	before, err := src.frame(1)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	marker := make([]byte, src.frameBytes)
	for i := range marker {
		marker[i] = 0xAB
	}
	if _, err := f.WriteAt(marker, src.valueAt+2*int64(src.frameBytes)); err != nil {
		t.Fatal(err)
	}
	f.Close()

	third, _ := src.frame(2)
	nf, _ := third.GetNativeFrame()
	if !reflect.DeepEqual(nf.RawDataSlice(), marker) {
		t.Errorf("frame 3 was not read from its own offset at the time it was asked for")
	}
	second, _ := src.frame(1)
	a, _ := before.GetNativeFrame()
	b, _ := second.GetNativeFrame()
	if !reflect.DeepEqual(a.RawDataSlice(), b.RawDataSlice()) {
		t.Errorf("frame 2 changed although its bytes did not")
	}
}

// The header read reports where the pixel data begins in every byte order it
// stops in, and -1 where it reads to the end.
func TestReadDicomHeaderStopOffset(t *testing.T) {
	for _, ts := range []string{uid.ExplicitVRLittleEndian, uid.ImplicitVRLittleEndian, uid.ExplicitVRBigEndian} {
		t.Run(transferSyntaxLabel(ts), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "f.dcm")
			writeNativeFixture(t, path, nativeFixture{ts: ts, rows: 4, cols: 4, spp: 1, bits: 16, frames: 1, withOverlay: true})
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			f, _ := os.Open(path)
			defer f.Close()
			_, stopAt, err := readDicomHeaderStop(f, int64(len(data)), path, nil)
			if err != nil {
				t.Fatal(err)
			}
			want := []byte{0xE0, 0x7F, 0x10, 0x00}
			if ts == uid.ExplicitVRBigEndian {
				want = []byte{0x7F, 0xE0, 0x00, 0x10}
			}
			if stopAt < 0 || stopAt+4 > int64(len(data)) || !reflect.DeepEqual(data[stopAt:stopAt+4], want) {
				t.Errorf("stopAt = %d does not point at the Pixel Data tag", stopAt)
			}
		})
	}
}
