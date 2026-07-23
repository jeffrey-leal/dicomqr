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
	tsJPEGBaseline = "1.2.840.10008.1.2.4.50"
	tsJPEGExtended = "1.2.840.10008.1.2.4.51"
	tsJPEG2000LL   = "1.2.840.10008.1.2.4.90"
	tsJPEG2000     = "1.2.840.10008.1.2.4.91"
)

// isUncompressedOnDisk reports whether uid is one of the two uncompressed
// little-endian syntaxes a profile can require.
func isUncompressedOnDisk(uid string) bool {
	return uid == tsImplicitVRLE || uid == tsExplicitVRLE
}

// canDecompressSyntax reports whether a built-in decoder exists for uid:
// JPEG Baseline/Extended via the Go JPEG decoder (the same path the viewer
// uses), JPEG 2000 via OpenJPEG when built with the openjpeg tag.
func canDecompressSyntax(uid string) bool {
	switch uid {
	case tsJPEGBaseline, tsJPEGExtended:
		return true
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
	tsUID := fileTransferSyntaxUID(path)
	if tsUID == "" {
		return false, errors.New("cannot determine transfer syntax")
	}
	if tsUID == targetTS {
		return false, nil
	}
	// A rewrite is required. An uncompressed source only needs a VR re-encode;
	// a compressed source must have a built-in decoder or we cannot proceed.
	if !isUncompressedOnDisk(tsUID) && !canDecompressSyntax(tsUID) {
		name := tsUID
		if n, known := unsupportedTransferSyntaxNames[tsUID]; known {
			name = n + " (" + tsUID + ")"
		}
		return false, fmt.Errorf("no built-in decoder for %s", name)
	}

	ds, err := sdicom.ParseFile(path, nil)
	if err != nil {
		return false, fmt.Errorf("parse: %w", err)
	}

	// Decompress encapsulated pixel data. Native pixel data and objects without
	// pixel data (e.g. SR documents) carry no encapsulated frames, so they fall
	// straight through to the re-encode below in the target VR.
	if pdElem, pdErr := ds.FindElementByTag(tag.PixelData); pdErr == nil {
		info, ok := pdElem.Value.GetValue().(sdicom.PixelDataInfo)
		if !ok {
			return false, errors.New("unexpected PixelData value type")
		}
		if info.IsEncapsulated {
			newInfo, colorOut, decErr := decompressPixelData(&ds, info, tsUID)
			if decErr != nil {
				return false, decErr
			}
			newPD, elemErr := sdicom.NewElement(tag.PixelData, newInfo)
			if elemErr != nil {
				return false, elemErr
			}
			replaceElement(&ds, newPD)
			if colorOut {
				// Both decoders emit interleaved RGB for colour frames.
				if err := setElementValue(&ds, tag.PhotometricInterpretation, []string{"RGB"}); err != nil {
					return false, err
				}
				if err := setElementValue(&ds, tag.PlanarConfiguration, []int{0}); err != nil {
					return false, err
				}
			}
		}
	}

	if err := setElementValue(&ds, tag.TransferSyntaxUID, []string{targetTS}); err != nil {
		return false, err
	}

	// Write to a temp file in the same directory, then rename over the
	// original (atomic on NTFS; MoveFileEx replaces existing files).
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".transcode_*.tmp")
	if err != nil {
		return false, err
	}
	tmpPath := tmp.Name()
	writeErr := sdicom.Write(tmp, ds, sdicom.SkipVRVerification(), sdicom.SkipValueTypeVerification())
	closeErr := tmp.Close()
	if writeErr != nil || closeErr != nil {
		os.Remove(tmpPath)
		if writeErr != nil {
			return false, fmt.Errorf("re-encode: %w", writeErr)
		}
		return false, closeErr
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return false, err
	}
	return true, nil
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
		var merged []byte
		for _, fr := range encFrames {
			if fr == nil || !fr.IsEncapsulated() {
				return info, false, errors.New("mixed native and encapsulated fragments")
			}
			merged = append(merged, fr.EncapsulatedData.Data...)
		}
		encFrames = []*frame.Frame{{Encapsulated: true, EncapsulatedData: frame.EncapsulatedFrame{Data: merged}}}
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
		if isJPEG2000TransferSyntax(tsUID) {
			nf, isColor, err = j2kFrameToNative(fr.EncapsulatedData.Data, bitsAlloc)
		} else {
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
