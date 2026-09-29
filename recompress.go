package main

// Lossless recompression after pixel masking — the write-side counterpart of
// transcode.go's decoders.
//
// Masking writes sample values and needs them native, so a compressed file a
// region applies to is decompressed first (processFile). What the masked
// frames become afterwards depends on what the source was:
//
//   - A losslessly-compressed source (JPEG 2000 Lossless, JPEG Lossless) is
//     encoded straight back into its own syntax — the export keeps the
//     encoding it arrived in instead of ballooning into Explicit VR LE.
//   - A lossy source (JPEG Baseline/Extended, lossy JPEG 2000) is NEVER
//     re-encoded lossily — a second lossy generation would degrade every
//     pixel in the image, not just the masked ones. Instead the decoded
//     samples are encoded to JPEG 2000 Lossless: no loss is added beyond the
//     decode masking already forced, the export stays compressed (measured
//     ~6:1 on echo), and the syntax change is disclosed in its own count
//     rather than folded into the round-trip one.
//
// The invariant: a re-encoded frame must decode back bit-identical to the
// masked native frame, or the encode fails and the file falls back to
// Explicit VR LE exactly as if no encoder existed (reported, as ever, in
// MaskDecompressed). Every fresh codestream is therefore decoded again with
// the same decoder the viewer trusts and compared sample for sample — the
// encoders are never taken on faith.

import (
	"errors"
	"fmt"
	"strings"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/frame"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// recompressTargetFor maps a masked file's source syntax to the syntax its
// pixels are re-encoded into, or ok=false when the only option is leaving
// the file uncompressed. A lossless source comes back as itself; a lossy
// source becomes JPEG 2000 Lossless — a disclosed syntax change in place of
// a silent quality change (see the file comment). Anything else (no decoder,
// or no encoder built in) leaves uncompressed as before.
func recompressTargetFor(sourceTS string) (string, bool) {
	switch sourceTS {
	case tsJPEG2000LL:
		return tsJPEG2000LL, jpeg2000Available
	case tsJPEGLossless, tsJPEGLosslessSV1:
		return sourceTS, jpegLosslessAvailable
	case tsJPEGBaseline, tsJPEGExtended, tsJPEG2000:
		return tsJPEG2000LL, jpeg2000Available
	}
	return "", false
}

// pixelStateSnapshot captures, immediately before a forced decompression,
// everything that decompression rewrites: the encapsulated pixel element and
// the attributes describing its encoding. Taken inside the masking block, so
// it sees the dataset after the profile's own edits — restoring it can never
// undo an edit.
type pixelStateSnapshot struct {
	sourceTS    string
	pixelData   *sdicom.Element
	photometric *sdicom.Element // nil when the file had none
	planarConf  *sdicom.Element // nil when the file had none
}

func snapshotPixelState(ds *sdicom.Dataset) pixelStateSnapshot {
	snap := pixelStateSnapshot{sourceTS: datasetTransferSyntaxUID(ds)}
	if e, err := ds.FindElementByTag(tag.PixelData); err == nil {
		snap.pixelData = e
	}
	if e, err := ds.FindElementByTag(tag.PhotometricInterpretation); err == nil {
		snap.photometric = e
	}
	if e, err := ds.FindElementByTag(tag.PlanarConfiguration); err == nil {
		snap.planarConf = e
	}
	return snap
}

func (s pixelStateSnapshot) photometricString() string {
	if s.photometric == nil {
		return ""
	}
	if strs, ok := s.photometric.Value.GetValue().([]string); ok && len(strs) > 0 {
		return strings.TrimSpace(strs[0])
	}
	return ""
}

// restoreOriginalPixels puts the pre-decompression pixel element and its
// describing attributes back verbatim — for a file the generous header gate
// admitted but the per-frame resolution then found nothing to mask on. The
// export carries the original codestream bytes, not a decode/encode of them.
// The one fallible step (building the transfer syntax element) happens before
// any mutation, so a failure leaves the dataset exactly as it was.
func (s pixelStateSnapshot) restoreOriginalPixels(ds *sdicom.Dataset) error {
	tsElem, err := sdicom.NewElement(tag.TransferSyntaxUID, []string{s.sourceTS})
	if err != nil {
		return err
	}
	if s.pixelData != nil {
		replaceElement(ds, s.pixelData)
	}
	replaceElement(ds, tsElem)
	s.restoreDescribingAttrs(ds)
	return nil
}

// restoreDescribingAttrs undoes what a colour decode rewrote around the
// pixels: the Photometric Interpretation / Planar Configuration it forces to
// RGB / 0. An attribute the file never had is removed rather than left
// behind. Infallible by construction — callers sequence it after their own
// fallible steps so the dataset is never left half-restored.
func (s pixelStateSnapshot) restoreDescribingAttrs(ds *sdicom.Dataset) {
	restore := func(t tag.Tag, e *sdicom.Element) {
		if e != nil {
			replaceElement(ds, e)
		} else {
			removeElementByTag(ds, t)
		}
	}
	restore(tag.PhotometricInterpretation, s.photometric)
	restore(tag.PlanarConfiguration, s.planarConf)
}

// removeElementByTag drops a top-level element if present.
func removeElementByTag(ds *sdicom.Dataset, t tag.Tag) {
	for i, e := range ds.Elements {
		if e.Tag == t {
			ds.Elements = append(ds.Elements[:i], ds.Elements[i+1:]...)
			return
		}
	}
}

// encodeMaskedFrame is the per-frame encode-and-verify, a variable so tests
// can force the fallback path without a broken codec build.
var encodeMaskedFrame = encodeAndVerifyFrame

// recompressPixelData re-encodes the dataset's native (masked) frames into
// targetTS (from recompressTargetFor). When target and source agree, the
// attributes the forced decompression rewrote are restored — the file ends
// exactly as it arrived. When the target differs (lossy source → JPEG 2000
// Lossless), the attributes keep describing what the pixels now are: the
// decode's RGB / PlanarConfiguration 0 stand for greyscale untouched, and a
// colour file is labelled YBR_RCT to match the reversible colour transform
// the encode applies. All fallible work — encoding, verification, element
// construction — happens before the first mutation, so on any error the
// dataset is exactly the decompressed one the caller already knows how to
// export.
func recompressPixelData(ds *sdicom.Dataset, snap pixelStateSnapshot, targetTS string, tokens cpuTokens) error {
	pdElem, err := ds.FindElementByTag(tag.PixelData)
	if err != nil {
		return fmt.Errorf("pixel data element: %w", err)
	}
	info, ok := pdElem.Value.GetValue().(sdicom.PixelDataInfo)
	if !ok {
		return fmt.Errorf("unexpected PixelData value type %T", pdElem.Value.GetValue())
	}
	if info.IsEncapsulated {
		return errors.New("pixel data is still encapsulated")
	}
	if len(info.Frames) == 0 {
		return errors.New("no frames to encode")
	}

	sameSyntax := targetTS == snap.sourceTS
	bitsAlloc := datasetInt(ds, tag.BitsAllocated, 8)
	prec := datasetInt(ds, tag.BitsStored, bitsAlloc)
	if prec < 2 || prec > 16 {
		return fmt.Errorf("bits stored %d is outside the encodable range 2-16", prec)
	}
	signed := datasetInt(ds, tag.PixelRepresentation, 0) == 1
	spp := datasetInt(ds, tag.SamplesPerPixel, 1)
	// Same syntax back: the reversible colour transform is an internal
	// property of the original codestream, so matching the original
	// photometric keeps declaration and content in the relationship the
	// source had. Cross-syntax to J2K Lossless: colour always takes the
	// reversible transform (and the YBR_RCT label below), which is both the
	// conformant labelling and the better compression.
	var mct bool
	if sameSyntax {
		mct = snap.photometricString() == "YBR_RCT"
	} else {
		mct = spp == 3
	}
	// SOF3 carries no signedness — samples travel as raw stored bits — while a
	// J2K codestream declares it, so its encoder wants real negative values.
	signBits := 0
	if signed && targetTS == tsJPEG2000LL {
		signBits = prec
	}

	// Each frame encodes and verifies independently — the expensive half of a
	// masked export — so a multi-frame file spreads across the run's idle
	// cores (forEachFrame). Every frame still goes through the decode-back
	// verification in encodeMaskedFrame, and any frame failing fails the file
	// exactly as the serial loop did, reporting the lowest-numbered one.
	encFrames := make([]*frame.Frame, len(info.Frames))
	err = forEachFrame(tokens, len(info.Frames), func(i int) error {
		fr := info.Frames[i]
		if fr == nil || fr.IsEncapsulated() {
			return fmt.Errorf("frame %d is not native", i+1)
		}
		nf, nerr := fr.GetNativeFrame()
		if nerr != nil {
			return fmt.Errorf("frame %d: %w", i+1, nerr)
		}
		planar, cols, rows, fspp, perr := planarSamplesFromNative(nf, signBits)
		if perr != nil {
			return fmt.Errorf("frame %d: %w", i+1, perr)
		}
		data, eerr := encodeMaskedFrame(targetTS, planar, cols, rows, fspp, prec, signed, mct)
		if eerr != nil {
			return fmt.Errorf("frame %d: %w", i+1, eerr)
		}
		// DICOM fragments must have even length (PS3.5 §A.4); JPEG-family
		// decoders ignore a pad byte after the end-of-stream marker.
		if len(data)%2 != 0 {
			data = append(data, 0)
		}
		encFrames[i] = &frame.Frame{
			Encapsulated:     true,
			EncapsulatedData: frame.EncapsulatedFrame{Data: data},
		}
		return nil
	})
	if err != nil {
		return err
	}

	newPD, err := sdicom.NewElement(tag.PixelData, sdicom.PixelDataInfo{
		IsEncapsulated: true,
		Frames:         encFrames,
	})
	if err != nil {
		return err
	}
	// Encapsulated pixel data is written with undefined length and VR OB; the
	// writer's fragment path triggers on the former, and NewElement supplies
	// neither. A nil offset table writes as the empty Basic Offset Table.
	newPD.ValueLength = tag.VLUndefinedLength
	newPD.RawValueRepresentation = "OB"
	tsElem, err := sdicom.NewElement(tag.TransferSyntaxUID, []string{targetTS})
	if err != nil {
		return err
	}
	var photElem *sdicom.Element
	if !sameSyntax && spp == 3 {
		photElem, err = sdicom.NewElement(tag.PhotometricInterpretation, []string{"YBR_RCT"})
		if err != nil {
			return err
		}
	}

	// Nothing below can fail: the dataset flips from decompressed to
	// recompressed in one piece or not at all.
	replaceElement(ds, newPD)
	replaceElement(ds, tsElem)
	if sameSyntax {
		snap.restoreDescribingAttrs(ds)
	} else if photElem != nil {
		replaceElement(ds, photElem)
	}
	return nil
}

// encodeAndVerifyFrame encodes one frame's planar samples into targetTS's
// codec and then proves the result lossless: the fresh codestream is decoded
// again and every sample compared. A mismatch — encoder clamping a
// nonconformant sample, an unexpected colour conversion, anything — returns
// an error, and the caller falls back to exporting uncompressed rather than
// shipping pixels that differ from the masked originals.
func encodeAndVerifyFrame(targetTS string, planar []int32, cols, rows, spp, prec int, signed, mct bool) ([]byte, error) {
	var (
		data []byte
		err  error
	)
	switch targetTS {
	case tsJPEG2000LL:
		data, err = encodeJPEG2000Lossless(planar, cols, rows, spp, prec, signed, mct)
	case tsJPEGLossless, tsJPEGLosslessSV1:
		data, err = encodeJPEGLossless(planar, cols, rows, spp, prec)
	default:
		return nil, fmt.Errorf("no lossless encoder for %s", transferSyntaxLabel(targetTS))
	}
	if err != nil {
		return nil, err
	}

	var (
		w, h, nc int
		got      []int32
	)
	switch targetTS {
	case tsJPEG2000LL:
		w, h, nc, _, _, got, err = decodeJPEG2000(data)
	default:
		w, h, nc, _, _, got, err = decodeJPEGLossless(data)
	}
	if err != nil {
		return nil, fmt.Errorf("verify decode: %w", err)
	}
	if w != cols || h != rows || nc != spp {
		return nil, fmt.Errorf("verify: geometry %dx%dx%d decoded as %dx%dx%d", cols, rows, spp, w, h, nc)
	}
	n := cols * rows * spp
	if len(got) < n {
		return nil, fmt.Errorf("verify: %d samples decoded, want %d", len(got), n)
	}
	for i := 0; i < n; i++ {
		if got[i] != planar[i] {
			return nil, fmt.Errorf("verify: sample %d decoded as %d, want %d — round trip is not lossless", i, got[i], planar[i])
		}
	}
	return data, nil
}

// planarSamplesFromNative reverses planarSamplesToNative: interleaved native
// samples out to numComps planes of int32 (plane 0 first), the layout both
// encoders take. Only frames produced by decompressPixelData reach this —
// always interleaved — because recompression only ever runs on files that
// were just decompressed. signBits > 0 sign-extends each sample's low
// signBits bits out of its unsigned container (two's complement survives the
// container truncation on decode, so this reverses it exactly).
func planarSamplesFromNative(nf frame.INativeFrame, signBits int) (samples []int32, cols, rows, spp int, err error) {
	cols, rows, spp = nf.Cols(), nf.Rows(), nf.SamplesPerPixel()
	pixels := cols * rows
	if pixels <= 0 || spp <= 0 {
		return nil, 0, 0, 0, fmt.Errorf("frame geometry %dx%dx%d", cols, rows, spp)
	}
	samples = make([]int32, pixels*spp)
	switch raw := nf.RawDataSlice().(type) {
	case []uint8:
		err = planarFromRaw(samples, raw, pixels, spp, signBits)
	case []uint16:
		err = planarFromRaw(samples, raw, pixels, spp, signBits)
	case []uint32:
		err = planarFromRaw(samples, raw, pixels, spp, signBits)
	case []int:
		err = planarFromRaw(samples, raw, pixels, spp, signBits)
	default:
		err = fmt.Errorf("unsupported pixel sample type %T", raw)
	}
	if err != nil {
		return nil, 0, 0, 0, err
	}
	return samples, cols, rows, spp, nil
}

func planarFromRaw[I pixelSample](dst []int32, raw []I, pixels, spp, signBits int) error {
	if want := pixels * spp; len(raw) < want {
		return fmt.Errorf("pixel buffer holds %d samples, need %d", len(raw), want)
	}
	var half, full int32
	if signBits > 0 {
		half = int32(1) << uint(signBits-1)
		full = int32(1) << uint(signBits)
	}
	for p := 0; p < pixels; p++ {
		for c := 0; c < spp; c++ {
			v := int32(raw[p*spp+c])
			if signBits > 0 && v >= half {
				v -= full
			}
			dst[c*pixels+p] = v
		}
	}
	return nil
}
