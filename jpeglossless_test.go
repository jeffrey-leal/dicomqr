//go:build jpeglossless

package main

import (
	_ "embed"
	"image"
	"os"
	"path/filepath"
	"testing"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/frame"
	"github.com/suyashkumar/dicom/pkg/tag"
	tagpkg "github.com/suyashkumar/dicom/pkg/tag"
)

// Fixtures generated once with libjpeg-turbo's cjpeg and verified with djpeg:
//   ramp8_rgb.ljpg    8×8  8-bit RGB, SV1 (cjpeg -lossless 1):        R=x·32, G=y·32, B=(x+y)·16
//   ramp16_gray.ljpg  8×8 16-bit gray, SV1 (cjpeg -precision 16 -lossless 1): v=(y·8+x)·1024
// The 16-bit ramp exceeds 32767 so the sign-extension path is exercised.

//go:embed testdata/ramp8_rgb.ljpg
var ramp8RGBLJPG []byte

//go:embed testdata/ramp16_gray.ljpg
var ramp16GrayLJPG []byte

// Lossless decode must reproduce the 8-bit RGB ramp bit-exactly, as planar
// planes in R, G, B order.
func TestDecodeJPEGLosslessRGB8(t *testing.T) {
	w, h, nc, prec, signed, samples, err := decodeJPEGLossless(ramp8RGBLJPG)
	if err != nil {
		t.Fatalf("decodeJPEGLossless: %v", err)
	}
	if w != 8 || h != 8 || nc != 3 || prec != 8 || signed {
		t.Fatalf("geometry = %dx%d nc=%d prec=%d signed=%v, want 8x8 nc=3 prec=8 signed=false", w, h, nc, prec, signed)
	}
	pixels := w * h
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			i := y*8 + x
			wantR, wantG, wantB := int32(x*32), int32(y*32), int32((x+y)*16)
			if samples[i] != wantR || samples[pixels+i] != wantG || samples[2*pixels+i] != wantB {
				t.Fatalf("pixel (%d,%d) = R%d G%d B%d, want R%d G%d B%d (lossless must be exact)",
					x, y, samples[i], samples[pixels+i], samples[2*pixels+i], wantR, wantG, wantB)
			}
		}
	}
}

// 16-bit grayscale must decode bit-exactly through the jpeg16 API path.
func TestDecodeJPEGLosslessGray16(t *testing.T) {
	w, h, nc, prec, _, samples, err := decodeJPEGLossless(ramp16GrayLJPG)
	if err != nil {
		t.Fatalf("decodeJPEGLossless: %v", err)
	}
	if w != 8 || h != 8 || nc != 1 || prec != 16 {
		t.Fatalf("geometry = %dx%d nc=%d prec=%d, want 8x8 nc=1 prec=16", w, h, nc, prec)
	}
	for i := 0; i < 64; i++ {
		if samples[i] != int32(i*1024) {
			t.Fatalf("sample[%d] = %d, want %d (lossless must be exact)", i, samples[i], i*1024)
		}
	}
}

// Colour frames are not windowable and carry the exact 8-bit channel values
// (prec 8 → the 2^prec−1 scale is the identity).
func TestDecodeJPEGLosslessFrameColor(t *testing.T) {
	df, err := decodeJPEGLosslessFrame(ramp8RGBLJPG, 1, 0, false, 0, 0, "RGB", false)
	if err != nil {
		t.Fatalf("decodeJPEGLosslessFrame: %v", err)
	}
	if df.colorImg == nil || df.windowable() {
		t.Fatal("colour frame must have colorImg set and not be windowable")
	}
	img, ok := df.colorImg.(*image.NRGBA)
	if !ok {
		t.Fatalf("colorImg is %T, want *image.NRGBA", df.colorImg)
	}
	// Pixel (7,0): R=224 G=0 B=112.
	if r, g, b := img.Pix[7*4], img.Pix[7*4+1], img.Pix[7*4+2]; r != 224 || g != 0 || b != 112 {
		t.Fatalf("pixel (7,0) = %d,%d,%d, want 224,0,112", r, g, b)
	}
}

// Monochrome frames window through the gray buffer; PixelRepresentation=1
// applies two's-complement sign extension at the frame's precision.
func TestDecodeJPEGLosslessFrameGray16(t *testing.T) {
	t.Run("unsigned", func(t *testing.T) {
		df, err := decodeJPEGLosslessFrame(ramp16GrayLJPG, 1, 0, false, 0, 0, "MONOCHROME2", false)
		if err != nil {
			t.Fatalf("decodeJPEGLosslessFrame: %v", err)
		}
		if !df.windowable() {
			t.Fatal("grayscale frame must be windowable")
		}
		for i := 0; i < 64; i++ {
			if df.displayValues()[i] != float32(i*1024) {
				t.Fatalf("gray[%d] = %v, want %v", i, df.displayValues()[i], float32(i*1024))
			}
		}
	})
	t.Run("signed", func(t *testing.T) {
		df, err := decodeJPEGLosslessFrame(ramp16GrayLJPG, 1, 0, false, 0, 0, "MONOCHROME2", true)
		if err != nil {
			t.Fatalf("decodeJPEGLosslessFrame: %v", err)
		}
		for i := 0; i < 64; i++ {
			v := int64(i * 1024)
			if v >= 32768 {
				v -= 65536
			}
			if df.displayValues()[i] != float32(v) {
				t.Fatalf("gray[%d] = %v, want %v (sign extension)", i, df.displayValues()[i], float32(v))
			}
		}
	})
}

// writeJPEGLosslessTestDICOM writes an 8×8 8-bit RGB file whose pixel data is
// the ramp8_rgb.ljpg stream encapsulated under the JPEG Lossless SV1 transfer
// syntax, deliberately SPLIT ACROSS TWO FRAGMENTS — the exact shape of the
// Philips EPIQ echo still captures that motivated this feature.
func writeJPEGLosslessTestDICOM(t *testing.T, dir string) string {
	t.Helper()
	codestream := append([]byte(nil), ramp8RGBLJPG...)
	if len(codestream)%2 != 0 {
		codestream = append(codestream, 0) // PS3.5 §A.4: final fragment padded to even length
	}
	// Split at an even offset so both fragments have even length.
	cut := (len(codestream) / 2) &^ 1
	frag1 := codestream[:cut]
	frag2 := codestream[cut:]

	pd, err := sdicom.NewElement(tag.PixelData, sdicom.PixelDataInfo{
		IsEncapsulated: true,
		Frames: []*frame.Frame{
			{Encapsulated: true, EncapsulatedData: frame.EncapsulatedFrame{Data: frag1}},
			{Encapsulated: true, EncapsulatedData: frame.EncapsulatedFrame{Data: frag2}},
		},
	})
	if err != nil {
		t.Fatalf("NewElement(PixelData): %v", err)
	}
	pd.ValueLength = tagpkg.VLUndefinedLength
	pd.RawValueRepresentation = "OB"

	ds := sdicom.Dataset{Elements: []*sdicom.Element{
		mustTestElement(t, tag.MediaStorageSOPClassUID, []string{"1.2.840.10008.5.1.4.1.1.7"}),
		mustTestElement(t, tag.MediaStorageSOPInstanceUID, []string{"1.2.3.4.7"}),
		mustTestElement(t, tag.TransferSyntaxUID, []string{"1.2.840.10008.1.2.4.70"}),
		mustTestElement(t, tag.SOPClassUID, []string{"1.2.840.10008.5.1.4.1.1.7"}),
		mustTestElement(t, tag.SOPInstanceUID, []string{"1.2.3.4.7"}),
		mustTestElement(t, tag.PhotometricInterpretation, []string{"RGB"}),
		mustTestElement(t, tag.Rows, []int{8}),
		mustTestElement(t, tag.Columns, []int{8}),
		mustTestElement(t, tag.BitsAllocated, []int{8}),
		mustTestElement(t, tag.BitsStored, []int{8}),
		mustTestElement(t, tag.HighBit, []int{7}),
		mustTestElement(t, tag.PixelRepresentation, []int{0}),
		mustTestElement(t, tag.SamplesPerPixel, []int{3}),
		mustTestElement(t, tag.PlanarConfiguration, []int{0}),
		mustTestElement(t, tag.NumberOfFrames, []string{"1"}),
		pd,
	}}

	path := filepath.Join(dir, "jls.dcm")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	if err := sdicom.Write(f, ds, sdicom.SkipVRVerification(), sdicom.SkipValueTypeVerification()); err != nil {
		t.Fatalf("write JPEG Lossless test DICOM: %v", err)
	}
	return path
}

// End-to-end through the viewer path: the pre-flight veto must let .70 pass,
// the two fragments must be reassembled, and the SV1 stream must decode.
func TestLoadDicomImageJPEGLosslessFragmented(t *testing.T) {
	path := writeJPEGLosslessTestDICOM(t, t.TempDir())
	st, err := loadDicomImage(path)
	if err != nil {
		t.Fatalf("loadDicomImage: %v", err)
	}
	b := st.img.Bounds()
	if b.Dx() != 8 || b.Dy() != 8 {
		t.Fatalf("image bounds = %dx%d, want 8x8", b.Dx(), b.Dy())
	}
	if st.frame == nil || st.frame.windowable() {
		t.Fatal("RGB lossless frame must be colour (not windowable)")
	}
	img, ok := st.frame.colorImg.(*image.NRGBA)
	if !ok {
		t.Fatalf("colorImg is %T, want *image.NRGBA", st.frame.colorImg)
	}
	if r, g, b := img.Pix[7*4], img.Pix[7*4+1], img.Pix[7*4+2]; r != 224 || g != 0 || b != 112 {
		t.Fatalf("pixel (7,0) = %d,%d,%d, want 224,0,112", r, g, b)
	}
}
