package main

// Uncompressed pixel data read one frame at a time, straight from the file.
//
// The library reads native pixel data one sample at a time — an io.ReadFull
// per sample, through its own reader — and a full parse reads every frame of
// the file whichever one is wanted. For a multi-frame acquisition stored
// uncompressed that made showing any single frame cost the whole file:
// field-measured 2026-10-20 on an echo study retrieved in Implicit VR Little
// Endian, 173 clips holding 11.6 GB, a clip's first image took 1.5 s on average
// and 13 s for the largest, and the viewer's filmstrip, which wants one frame
// per clip, spent 252 s of CPU reading all of them. Native pixel data is fixed
// size and in order, so frame i sits at a computable offset: the header is read
// without the pixel data (readDicomHeaderStop says where it begins), and each
// frame is read with one ReadAt when it is asked for.
//
// Only a file the library would read exactly the same way takes this path —
// Explicit or Implicit VR Little Endian, 8 or 16 bits allocated, the frame
// geometry stated with the value types the library requires, and a Pixel Data
// length its own check accepts. Anything else (compressed, big endian,
// deflated, 1- or 32-bit, a mismatched length, a truncated file) returns
// ok=false and the caller parses in full, so such a file shows the image, or
// fails with the error, it always did. The frames built here hold the values
// the library's readNativeFrame would produce, sample for sample
// (TestNativeFramesMatchFullParse).
//
// The file is reopened for each frame rather than held open: a viewer window
// keeps its parsed file for as long as it is open, and on Windows an open file
// cannot be deleted from Local Browse.

import (
	"encoding/binary"
	"fmt"
	"os"
	"strconv"

	sdicom "github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/frame"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// nativeFrameSource locates the frames of one file's uncompressed pixel data.
type nativeFrameSource struct {
	path string
	// valueAt is the file offset of the first frame's first byte.
	valueAt    int64
	frameBytes int
	frames     int
	rows, cols int
	spp        int
	bitsAlloc  int
}

// openNativeFrameSource reads path's header and, when its pixel data can be
// read frame by frame (see the file comment), returns the header with a source
// for the frames. ok=false means: parse the file in full instead.
func openNativeFrameSource(path string) (ds sdicom.Dataset, src *nativeFrameSource, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return sdicom.Dataset{}, nil, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return sdicom.Dataset{}, nil, false
	}
	// Declined on the File Meta when the syntax is any other: a compressed file
	// is parsed in full next, and reading its whole header twice cost about as
	// much again as that parse (measured on JPEG 2000 CT slices).
	ds, stopAt, err := readDicomHeaderStop(f, info.Size(), path, func(meta *sdicom.Dataset) bool {
		ts := datasetTransferSyntaxUID(meta)
		return ts == tsImplicitVRLE || ts == tsExplicitVRLE
	})
	if err != nil || stopAt < 0 {
		return sdicom.Dataset{}, nil, false
	}
	explicit := datasetTransferSyntaxUID(&ds) == tsExplicitVRLE

	// The geometry, read as readNativeFrames reads it: the four integers must
	// carry integer values, and NumberOfFrames, when present, a string that
	// parses as it stands — the library uses strconv.Atoi on it untrimmed.
	rows, okR := headerInt(&ds, tag.Rows)
	cols, okC := headerInt(&ds, tag.Columns)
	bits, okB := headerInt(&ds, tag.BitsAllocated)
	spp, okS := headerInt(&ds, tag.SamplesPerPixel)
	if !okR || !okC || !okB || !okS || rows <= 0 || cols <= 0 || spp <= 0 || (bits != 8 && bits != 16) {
		return sdicom.Dataset{}, nil, false
	}
	frames := 1
	if e, ferr := ds.FindElementByTag(tag.NumberOfFrames); ferr == nil {
		strs, isStr := e.Value.GetValue().([]string)
		if !isStr || len(strs) == 0 {
			return sdicom.Dataset{}, nil, false
		}
		n, aerr := strconv.Atoi(strs[0])
		if aerr != nil || n <= 0 {
			return sdicom.Dataset{}, nil, false
		}
		frames = n
	}

	// The Pixel Data element's own header: tag, then VR and a 4-byte length
	// (Explicit VR, OB or OW) or the length alone (Implicit VR).
	hdr := make([]byte, 12)
	if _, err := f.ReadAt(hdr, stopAt); err != nil {
		return sdicom.Dataset{}, nil, false
	}
	if binary.LittleEndian.Uint16(hdr[0:]) != tag.PixelData.Group || binary.LittleEndian.Uint16(hdr[2:]) != tag.PixelData.Element {
		return sdicom.Dataset{}, nil, false // Float Pixel Data, an Extended Offset Table, …
	}
	var vl uint32
	var valueAt int64
	if explicit {
		if vr := string(hdr[4:6]); vr != "OB" && vr != "OW" {
			return sdicom.Dataset{}, nil, false
		}
		vl, valueAt = binary.LittleEndian.Uint32(hdr[8:]), stopAt+12
	} else {
		vl, valueAt = binary.LittleEndian.Uint32(hdr[4:]), stopAt+8
	}
	if vl == 0xFFFFFFFF {
		return sdicom.Dataset{}, nil, false // undefined length: encapsulated after all
	}

	// readNativeFrames' own length rule: exactly the frames, or the frames plus
	// one padding byte making an even length. Anything else is its error to give.
	frameBytes := rows * cols * spp * (bits / 8)
	total := uint64(frameBytes) * uint64(frames)
	if total != uint64(vl) && !(total+1 == uint64(vl) && vl%2 == 0) {
		return sdicom.Dataset{}, nil, false
	}
	if valueAt+int64(total) > info.Size() {
		return sdicom.Dataset{}, nil, false // truncated: the full parse reports it
	}
	return ds, &nativeFrameSource{
		path: path, valueAt: valueAt, frameBytes: frameBytes, frames: frames,
		rows: rows, cols: cols, spp: spp, bitsAlloc: bits,
	}, true
}

// headerInt reads an integer element the way the library's MustGetInts does:
// only an []int value counts.
func headerInt(ds *sdicom.Dataset, t tag.Tag) (int, bool) {
	e, err := ds.FindElementByTag(t)
	if err != nil {
		return 0, false
	}
	v, ok := e.Value.GetValue().([]int)
	if !ok || len(v) == 0 {
		return 0, false
	}
	return v[0], true
}

// frame reads frame i (0-based, in range) from the file, as the native frame
// the library would have built for it. Safe for concurrent use: each call opens
// the file for itself.
func (s *nativeFrameSource) frame(i int) (*frame.Frame, error) {
	f, err := os.Open(s.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, s.frameBytes)
	if _, err := f.ReadAt(buf, s.valueAt+int64(i)*int64(s.frameBytes)); err != nil {
		return nil, fmt.Errorf("read frame %d: %w", i+1, err)
	}
	if s.bitsAlloc == 8 {
		// The bytes are the samples; no copy.
		return &frame.Frame{NativeData: &frame.NativeFrame[uint8]{
			RawData:                 buf,
			InternalSamplesPerPixel: s.spp,
			InternalRows:            s.rows,
			InternalCols:            s.cols,
			InternalBitsPerSample:   s.bitsAlloc,
		}}, nil
	}
	nf := frame.NewNativeFrame[uint16](s.bitsAlloc, s.rows, s.cols, s.rows*s.cols, s.spp)
	for k := range nf.RawData {
		nf.RawData[k] = binary.LittleEndian.Uint16(buf[2*k:])
	}
	return &frame.Frame{NativeData: nf}, nil
}
