package main

import (
	"errors"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/frame"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// Compressed transfer syntaxes the built-in decoders can convert to the
// required uncompressed syntax (PS3.5 §10.1).
const (
	tsJPEGBaseline    = "1.2.840.10008.1.2.4.50"
	tsJPEGExtended    = "1.2.840.10008.1.2.4.51"
	tsJPEGLossless    = "1.2.840.10008.1.2.4.57"
	tsJPEGLosslessSV1 = "1.2.840.10008.1.2.4.70"
	tsJPEG2000LL      = "1.2.840.10008.1.2.4.90"
	tsJPEG2000        = "1.2.840.10008.1.2.4.91"
)

// isUncompressedOnDisk reports whether uid is one of the two uncompressed
// little-endian syntaxes a profile can require.
func isUncompressedOnDisk(uid string) bool {
	return uid == tsImplicitVRLE || uid == tsExplicitVRLE
}

// canDecompressSyntax reports whether a built-in decoder exists for uid:
// JPEG Baseline/Extended via the Go JPEG decoder (the same path the viewer
// uses), JPEG 2000 via OpenJPEG when built with the openjpeg tag, JPEG Lossless
// via libjpeg-turbo when built with the jpeglossless tag.
//
// JPEG Lossless is decodable here but is deliberately absent from
// acceptedSyntaxesFor, so it is never negotiated: this gate is reached for it
// only by a modification profile converting a file already on disk. A server
// therefore cannot be induced to send it, and retrieve behaviour is unchanged.
func canDecompressSyntax(uid string) bool {
	switch uid {
	case tsJPEGBaseline, tsJPEGExtended:
		return true
	case tsJPEGLossless, tsJPEGLosslessSV1:
		return jpegLosslessAvailable
	case tsJPEG2000LL, tsJPEG2000:
		return jpeg2000Available
	}
	return false
}

// acceptedSyntaxesFor returns the transfer syntaxes negotiable when requiredTS
// is demanded on disk, in preference order: the required syntax itself first —
// a transcoding-capable server picks it and nothing needs converting — then
// every syntax the receive path can convert locally (the other uncompressed
// VR is a lossless re-encode; the compressed set is decoded with the viewer's
// decoders). A server limited to a syntax outside this list cannot deliver
// and the retrieve fails visibly. Returns nil when requiredTS is empty
// (as stored — accept everything).
func acceptedSyntaxesFor(requiredTS string) []string {
	if requiredTS == "" {
		return nil
	}
	accepted := []string{requiredTS}
	if requiredTS == tsImplicitVRLE {
		accepted = append(accepted, tsExplicitVRLE)
	} else {
		accepted = append(accepted, tsImplicitVRLE)
	}
	accepted = append(accepted, tsJPEGBaseline, tsJPEGExtended)
	if jpeg2000Available {
		accepted = append(accepted, tsJPEG2000LL, tsJPEG2000)
	}
	return accepted
}

// checkDecodableSource reports whether a file stored in sourceTS can be
// converted at all: an uncompressed source only needs a VR re-encode, while a
// compressed one needs a built-in decoder. The error names the syntax, since
// that is what tells the user whether the operation can ever succeed.
func checkDecodableSource(sourceTS string) error {
	if isUncompressedOnDisk(sourceTS) || canDecompressSyntax(sourceTS) {
		return nil
	}
	name := sourceTS
	if n, known := unsupportedTransferSyntaxNames[sourceTS]; known {
		name = n + " (" + sourceTS + ")"
	}
	return fmt.Errorf("no built-in decoder for %s", name)
}

// datasetTransferSyntaxUID reads (0002,0010) from an already-parsed dataset.
// The file-based fileTransferSyntaxUID cannot serve callers that hold only a
// dataset — the modification engine parses each file once and never revisits
// it on disk. Returns "" when the element is absent or not a string.
func datasetTransferSyntaxUID(ds *sdicom.Dataset) string {
	elem, err := ds.FindElementByTag(tag.TransferSyntaxUID)
	if err != nil {
		return ""
	}
	if strs, ok := elem.Value.GetValue().([]string); ok && len(strs) > 0 {
		return strings.TrimSpace(strs[0])
	}
	return ""
}

// convertDatasetSyntax rewrites an in-memory dataset so its transfer syntax is
// targetTS — one of the two uncompressed on-disk syntaxes. Encapsulated
// (compressed) pixel data is decompressed with the viewer's decoders; a dataset
// already uncompressed in the other VR encoding needs no pixel work at all,
// since the VR conversion happens when the writer encodes it under the new
// syntax. Objects without pixel data (e.g. SR documents) likewise just change
// syntax.
//
// Returns (false, nil) when sourceTS already equals targetTS, (true, nil) after
// a successful conversion, and (false, err) when the source is compressed with
// no built-in decoder or a frame fails to decode. ds is left partially modified
// on an error path, so callers must discard it rather than write it out.
//
// This is the whole conversion: transcodeDICOMFileToTemp wraps it in file I/O
// for the receive path, and processFile calls it directly on the dataset it has
// already transformed.
func convertDatasetSyntax(ds *sdicom.Dataset, sourceTS, targetTS string) (bool, error) {
	if sourceTS == "" {
		return false, errors.New("cannot determine transfer syntax")
	}
	if sourceTS == targetTS {
		return false, nil
	}
	// A rewrite is required. An uncompressed source only needs a VR re-encode;
	// a compressed source must have a built-in decoder or we cannot proceed.
	if err := checkDecodableSource(sourceTS); err != nil {
		return false, err
	}

	// Decompress encapsulated pixel data. Native pixel data and objects without
	// pixel data carry no encapsulated frames, so they fall straight through to
	// the re-encode in the target VR.
	if pdElem, pdErr := ds.FindElementByTag(tag.PixelData); pdErr == nil {
		info, ok := pdElem.Value.GetValue().(sdicom.PixelDataInfo)
		if !ok {
			return false, errors.New("unexpected PixelData value type")
		}
		if info.IsEncapsulated {
			newInfo, colorOut, decErr := decompressPixelData(ds, info, sourceTS)
			if decErr != nil {
				return false, decErr
			}
			newPD, elemErr := sdicom.NewElement(tag.PixelData, newInfo)
			if elemErr != nil {
				return false, elemErr
			}
			replaceElement(ds, newPD)
			if colorOut {
				// Every decoder emits interleaved RGB for colour frames.
				if err := setElementValue(ds, tag.PhotometricInterpretation, []string{"RGB"}); err != nil {
					return false, err
				}
				if err := setElementValue(ds, tag.PlanarConfiguration, []int{0}); err != nil {
					return false, err
				}
			}
		}
	}

	if err := setElementValue(ds, tag.TransferSyntaxUID, []string{targetTS}); err != nil {
		return false, err
	}
	return true, nil
}

// transcodeDICOMFile rewrites a DICOM file in place so its transfer syntax is
// targetTS — one of the two uncompressed on-disk syntaxes (Implicit or Explicit
// VR LE). Encapsulated (compressed) pixel data is decompressed with the
// viewer's decoders; a file that is already uncompressed but in the *other* VR
// encoding is re-encoded (a lossless VR conversion, no pixel decode). The
// receive path runs this on every incoming file before it reaches its final
// destination, so an entire retrieve lands in a single transfer syntax.
//
// Returns (false, nil) when the file is already in targetTS, (true, nil) after a
// successful rewrite, and (false, err) when a compressed syntax has no built-in
// decoder or the rewrite fails — the original file is left untouched on every
// error path.
func transcodeDICOMFile(path, targetTS string) (bool, error) {
	tmpPath, changed, err := transcodeDICOMFileToTemp(path, targetTS, filepath.Dir(path))
	if err != nil || !changed {
		return false, err
	}
	// Rename over the original (atomic on NTFS; MoveFileEx replaces existing
	// files) — the temp was created in the same directory for this reason.
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return false, err
	}
	return true, nil
}

// transcodeDICOMFileToTemp converts a COPY of path to targetTS, written as a
// temp file in tmpDir; the source file is never touched. Returns ("", false,
// nil) when the file is already in targetTS. On success the caller owns the
// returned temp file and must remove or rename it.
func transcodeDICOMFileToTemp(path, targetTS, tmpDir string) (string, bool, error) {
	tsUID := fileTransferSyntaxUID(path)
	if tsUID == "" {
		return "", false, errors.New("cannot determine transfer syntax")
	}
	if tsUID == targetTS {
		return "", false, nil
	}
	// Reject an undecodable source before parsing it. convertDatasetSyntax
	// checks this too, but only after the parse: doing it here keeps the cost
	// off files that cannot be converted anyway, and keeps "no built-in decoder
	// for JPEG-LS Lossless" as the reported reason rather than whatever the
	// parse of an unreadable file happens to say first.
	if err := checkDecodableSource(tsUID); err != nil {
		return "", false, err
	}

	ds, err := safeParseFile(path, nil)
	if err != nil {
		return "", false, fmt.Errorf("parse: %w", err)
	}
	if _, err := convertDatasetSyntax(&ds, tsUID, targetTS); err != nil {
		return "", false, err
	}

	tmp, err := os.CreateTemp(tmpDir, ".transcode_*.tmp")
	if err != nil {
		return "", false, err
	}
	tmpPath := tmp.Name()
	writeErr := sdicom.Write(tmp, ds, sdicom.SkipVRVerification(), sdicom.SkipValueTypeVerification())
	closeErr := tmp.Close()
	if writeErr != nil || closeErr != nil {
		os.Remove(tmpPath)
		if writeErr != nil {
			return "", false, fmt.Errorf("re-encode: %w", writeErr)
		}
		return "", false, closeErr
	}
	return tmpPath, true, nil
}

// mergeEncapsulatedFragments reassembles the fragments of one encapsulated
// single-frame image into a single frame holding the complete codestream.
// PS3.5 §A.4: fragment boundaries are arbitrary splits of one stream, so
// plain in-order concatenation restores it (only the final fragment may carry
// a padding byte, which JPEG-family decoders ignore after EOI).
func mergeEncapsulatedFragments(frames []*frame.Frame) (*frame.Frame, error) {
	var merged []byte
	for _, fr := range frames {
		if fr == nil || !fr.IsEncapsulated() {
			return nil, errors.New("mixed native and encapsulated fragments")
		}
		merged = append(merged, fr.EncapsulatedData.Data...)
	}
	return &frame.Frame{Encapsulated: true, EncapsulatedData: frame.EncapsulatedFrame{Data: merged}}, nil
}

// decompressPixelData converts encapsulated frames to native frames. colorOut
// reports whether any frame decoded to colour (the caller then rewrites the
// Photometric Interpretation as RGB).
func decompressPixelData(ds *sdicom.Dataset, info sdicom.PixelDataInfo, tsUID string) (sdicom.PixelDataInfo, bool, error) {
	bitsAlloc := datasetInt(ds, tag.BitsAllocated, 16)
	numberOfFrames := datasetInt(ds, tag.NumberOfFrames, 1)

	// A single-frame image may legally arrive split across several fragments;
	// suyashkumar exposes each fragment as a "frame". Reassemble the full
	// codestream in that case. A multi-frame image whose fragment count does
	// not match its frame count cannot be mapped reliably — bail out.
	encFrames := info.Frames
	if numberOfFrames == 1 && len(encFrames) > 1 {
		mergedFrame, err := mergeEncapsulatedFragments(encFrames)
		if err != nil {
			return info, false, err
		}
		encFrames = []*frame.Frame{mergedFrame}
	} else if len(encFrames) != numberOfFrames {
		return info, false, fmt.Errorf("%d fragments for %d frames — cannot map fragments to frames", len(encFrames), numberOfFrames)
	}

	colorOut := false
	newFrames := make([]*frame.Frame, 0, len(encFrames))
	for i, fr := range encFrames {
		if fr == nil || !fr.IsEncapsulated() {
			return info, false, errors.New("mixed native and encapsulated frames")
		}
		var nf frame.INativeFrame
		var isColor bool
		var err error
		switch {
		case isJPEG2000TransferSyntax(tsUID):
			nf, isColor, err = j2kFrameToNative(fr.EncapsulatedData.Data, bitsAlloc)
		case isJPEGLosslessTransferSyntax(tsUID):
			nf, isColor, err = jpegLosslessFrameToNative(fr.EncapsulatedData.Data, bitsAlloc)
		default:
			nf, isColor, err = jpegFrameToNative(fr, bitsAlloc)
		}
		if err != nil {
			return info, false, fmt.Errorf("frame %d: %w", i+1, err)
		}
		colorOut = colorOut || isColor
		newFrames = append(newFrames, &frame.Frame{Encapsulated: false, NativeData: nf})
	}
	return sdicom.PixelDataInfo{Frames: newFrames, IsEncapsulated: false}, colorOut, nil
}

// j2kFrameToNative decodes one JPEG 2000 codestream into a native frame whose
// container width matches the dataset's BitsAllocated. Colour output is
// interleaved RGB (OpenJPEG applies the inverse RCT/ICT itself, so components
// arrive as RGB planes).
func j2kFrameToNative(data []byte, bitsAlloc int) (frame.INativeFrame, bool, error) {
	w, h, nc, _, _, samples, err := decodeJPEG2000(data)
	if err != nil {
		return nil, false, err
	}
	return planarSamplesToNative(w, h, nc, bitsAlloc, samples)
}

// jpegLosslessFrameToNative decodes one JPEG Lossless (SOF3) codestream. The
// libjpeg-turbo decoder returns planar int32 samples on the same contract as
// decodeJPEG2000, so the two share planarSamplesToNative.
//
// Reachable only from a modification profile converting a file already on disk
// — JPEG Lossless is never negotiated (see canDecompressSyntax).
func jpegLosslessFrameToNative(data []byte, bitsAlloc int) (frame.INativeFrame, bool, error) {
	w, h, nc, _, _, samples, err := decodeJPEGLossless(data)
	if err != nil {
		return nil, false, err
	}
	return planarSamplesToNative(w, h, nc, bitsAlloc, samples)
}

// planarSamplesToNative packs planar decoder output into a native frame,
// interleaving the components of a colour image into RGB triplets. The bool
// reports colour output, which makes the caller rewrite Photometric
// Interpretation.
func planarSamplesToNative(w, h, nc, bitsAlloc int, samples []int32) (frame.INativeFrame, bool, error) {
	pixels := w * h
	if pixels <= 0 || len(samples) < pixels*nc {
		return nil, false, errors.New("decoded sample buffer too small")
	}

	if nc >= 3 {
		nf, err := newNativeFromSamples(bitsAlloc, h, w, 3, func(i int) int32 {
			pixel, comp := i/3, i%3
			return samples[comp*pixels+pixel]
		})
		return nf, true, err
	}

	nf, err := newNativeFromSamples(bitsAlloc, h, w, 1, func(i int) int32 { return samples[i] })
	return nf, false, err
}

// jpegFrameToNative decodes one JPEG Baseline/Extended frame via the library's
// Go JPEG decoder (the same path the viewer's GetImage uses).
func jpegFrameToNative(fr *frame.Frame, bitsAlloc int) (frame.INativeFrame, bool, error) {
	img, err := fr.GetImage()
	if err != nil {
		return nil, false, fmt.Errorf("JPEG decode: %w", err)
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil, false, errors.New("JPEG decode: empty image")
	}

	switch im := img.(type) {
	case *image.Gray:
		nf, err := newNativeFromSamples(bitsAlloc, h, w, 1, func(i int) int32 {
			return int32(im.Pix[(i/w)*im.Stride+(i%w)])
		})
		return nf, false, err
	case *image.Gray16:
		nf, err := newNativeFromSamples(bitsAlloc, h, w, 1, func(i int) int32 {
			return int32(im.Gray16At(b.Min.X+i%w, b.Min.Y+i/w).Y)
		})
		return nf, false, err
	default:
		// Colour (YCbCr from the JPEG decoder) → interleaved 8-bit RGB.
		nf, err := newNativeFromSamples(bitsAlloc, h, w, 3, func(i int) int32 {
			pixel, comp := i/3, i%3
			r, g, bl, _ := img.At(b.Min.X+pixel%w, b.Min.Y+pixel/w).RGBA()
			switch comp {
			case 0:
				return int32(r >> 8)
			case 1:
				return int32(g >> 8)
			default:
				return int32(bl >> 8)
			}
		})
		return nf, true, err
	}
}

// newNativeFromSamples builds a NativeFrame of the width the dataset's
// BitsAllocated dictates, filling it from sample(i) where i indexes the
// interleaved sample stream (pixel-major, then component). Signed values
// survive as two's complement under the uint truncation.
func newNativeFromSamples(bitsAlloc, rows, cols, spp int, sample func(i int) int32) (frame.INativeFrame, error) {
	n := rows * cols * spp
	switch bitsAlloc {
	case 8:
		nf := frame.NewNativeFrame[uint8](8, rows, cols, rows*cols, spp)
		for i := 0; i < n; i++ {
			nf.RawData[i] = uint8(sample(i))
		}
		return nf, nil
	case 16:
		nf := frame.NewNativeFrame[uint16](16, rows, cols, rows*cols, spp)
		for i := 0; i < n; i++ {
			nf.RawData[i] = uint16(sample(i))
		}
		return nf, nil
	case 32:
		nf := frame.NewNativeFrame[uint32](32, rows, cols, rows*cols, spp)
		for i := 0; i < n; i++ {
			nf.RawData[i] = uint32(sample(i))
		}
		return nf, nil
	}
	return nil, fmt.Errorf("unsupported BitsAllocated %d", bitsAlloc)
}

// projectedPixelBytes estimates what a file's pixel data will occupy in memory
// once decoded, from its header alone — Columns × Rows × SamplesPerPixel ×
// ceil(BitsAllocated/8) × NumberOfFrames.
//
// It exists because decompressed size, not file size, is what a run has to be
// sized against: decompressPixelData holds every frame of a file at once, and a
// multi-frame acquisition that is a few tens of megabytes on disk can be
// hundreds decoded. The mask benchmark has admitted files against this figure
// since it was written; runModificationImpl weighs its worker pool by it.
//
// Returns 0 for a dataset with no pixel geometry (a report, a key-object
// selection, or a header that would not parse), which is the honest answer:
// nothing about it says how much memory its pixels need, because it has none.
func projectedPixelBytes(ds *sdicom.Dataset) int64 {
	cols := int64(datasetInt(ds, tag.Columns, 0))
	rows := int64(datasetInt(ds, tag.Rows, 0))
	if cols <= 0 || rows <= 0 {
		return 0
	}
	spp := int64(datasetInt(ds, tag.SamplesPerPixel, 1))
	bytesPerSample := int64(datasetInt(ds, tag.BitsAllocated, 8)+7) / 8
	frames := int64(datasetInt(ds, tag.NumberOfFrames, 1))
	if spp <= 0 || bytesPerSample <= 0 || frames <= 0 {
		return 0
	}
	return cols * rows * spp * bytesPerSample * frames
}

// datasetInt reads the first integer value of a tag, tolerating the IS
// (integer string) representation NumberOfFrames uses.
func datasetInt(ds *sdicom.Dataset, t tag.Tag, def int) int {
	e, err := ds.FindElementByTag(t)
	if err != nil {
		return def
	}
	switch v := e.Value.GetValue().(type) {
	case []int:
		if len(v) > 0 {
			return v[0]
		}
	case []string:
		if len(v) > 0 {
			if n, convErr := strconv.Atoi(strings.TrimSpace(v[0])); convErr == nil {
				return n
			}
		}
	}
	return def
}

// replaceElement swaps the element with newElem's tag in place, appending when
// the tag is not present.
func replaceElement(ds *sdicom.Dataset, newElem *sdicom.Element) {
	for i, e := range ds.Elements {
		if e.Tag == newElem.Tag {
			ds.Elements[i] = newElem
			return
		}
	}
	ds.Elements = append(ds.Elements, newElem)
}

// setElementValue replaces (or adds) an element with the given value.
func setElementValue(ds *sdicom.Dataset, t tag.Tag, data any) error {
	e, err := sdicom.NewElement(t, data)
	if err != nil {
		return err
	}
	replaceElement(ds, e)
	return nil
}
