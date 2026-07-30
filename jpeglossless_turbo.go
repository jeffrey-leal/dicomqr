//go:build jpeglossless

// Package-level JPEG Lossless (ITU-T T.81 process 14, SOF3) decoder backed by
// libjpeg-turbo 3.x, linked statically. Lossless decoding — including the
// 12/16-bit sample APIs — is a first-class libjpeg-turbo feature since 3.0.
// Built only when the "jpeglossless" build tag is set; otherwise
// jpeglossless_stub.go provides a stub that reports the format as unsupported.
//
// Requires the MSYS2 package: pacman -S mingw-w64-x86_64-libjpeg-turbo
package main

/*
#cgo CFLAGS: -IC:/msys64/mingw64/include
#cgo LDFLAGS: -LC:/msys64/mingw64/lib -l:libjpeg.a

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdint.h>
#include <setjmp.h>
#include <jpeglib.h>
#include <jerror.h>

typedef struct {
    int ok;
    int width;
    int height;
    int numcomps;
    int prec;
    int sgnd;
    int32_t *samples; // planar: numcomps planes of width*height, malloc'd; free with dq_jls_free
    char err[256];
} dq_jls_result;

static void dq_jls_err_set(dq_jls_result *r, const char *msg) {
    r->ok = 0;
    strncpy(r->err, msg, sizeof(r->err) - 1);
    r->err[sizeof(r->err) - 1] = 0;
}

static void dq_jls_free(int32_t *p) { free(p); }

// Error manager: libjpeg's default error_exit calls exit(); replace it with a
// longjmp back into dq_decode_jls so a malformed stream becomes a Go error.
typedef struct {
    struct jpeg_error_mgr pub;
    jmp_buf jb;
    char msg[JMSG_LENGTH_MAX];
} dq_jls_err_mgr;

static void dq_jls_error_exit(j_common_ptr cinfo) {
    dq_jls_err_mgr *e = (dq_jls_err_mgr *)cinfo->err;
    (*cinfo->err->format_message)(cinfo, e->msg);
    longjmp(e->jb, 1);
}

// Quietly swallow libjpeg warnings (same policy as the OpenJPEG decoder).
static void dq_jls_output_message(j_common_ptr cinfo) { (void)cinfo; }

// dq_decode_jls decodes one JPEG stream (lossless SOF3 is the intended input;
// any precision 2..16, grayscale or 3-component) into planar int32 samples.
// The entire libjpeg lifecycle lives inside this single call frame so the
// longjmp never crosses a Go stack frame.
static void dq_decode_jls(const unsigned char *data, int len, dq_jls_result *r) {
    memset(r, 0, sizeof(*r));
    if (len < 4) { dq_jls_err_set(r, "JPEG stream too short"); return; }

    struct jpeg_decompress_struct cinfo;
    dq_jls_err_mgr jerr;
    // Pointers freed in the longjmp handler must be volatile: locals modified
    // between setjmp and longjmp are otherwise indeterminate afterwards.
    int32_t * volatile out = NULL;
    void * volatile rowbuf = NULL;

    cinfo.err = jpeg_std_error(&jerr.pub);
    jerr.pub.error_exit = dq_jls_error_exit;
    jerr.pub.output_message = dq_jls_output_message;
    jerr.msg[0] = 0;
    if (setjmp(jerr.jb)) {
        free((void *)out);
        free((void *)rowbuf);
        jpeg_destroy_decompress(&cinfo);
        dq_jls_err_set(r, jerr.msg[0] ? jerr.msg : "JPEG decode failed");
        return;
    }

    jpeg_create_decompress(&cinfo);
    jpeg_mem_src(&cinfo, data, (unsigned long)len);
    jpeg_read_header(&cinfo, TRUE);

    // Lossless mode supports no colour conversion: out_color_space must equal
    // jpeg_color_space or start_decompress error-exits (JERR_CONVERSION_NOTIMPL).
    // Samples therefore pass through untouched — for DICOM Photometric
    // Interpretation RGB (the Philips echo case) pass-through IS RGB. A
    // hypothetical YBR lossless file would come through unconverted.
    cinfo.out_color_space = cinfo.jpeg_color_space;

    jpeg_start_decompress(&cinfo);

    int w = (int)cinfo.output_width;
    int h = (int)cinfo.output_height;
    int nc = (int)cinfo.output_components;
    int prec = (int)cinfo.data_precision;
    if (w <= 0 || h <= 0 || nc <= 0 || nc > 4 || prec < 2 || prec > 16) {
        jpeg_destroy_decompress(&cinfo);
        dq_jls_err_set(r, "unsupported JPEG geometry");
        return;
    }
    size_t pixels = (size_t)w * (size_t)h;
    size_t total = pixels * (size_t)nc;
    if (pixels == 0 || total / (size_t)nc != pixels || total > (size_t)1 << 28) {
        jpeg_destroy_decompress(&cinfo);
        dq_jls_err_set(r, "JPEG dimensions too large");
        return;
    }
    out = (int32_t *)malloc(total * sizeof(int32_t));
    rowbuf = malloc((size_t)w * (size_t)nc * 2); // 2 bytes/sample covers 8/12/16-bit
    if (out == NULL || rowbuf == NULL) {
        free((void *)out);
        free((void *)rowbuf);
        jpeg_destroy_decompress(&cinfo);
        dq_jls_err_set(r, "out of memory");
        return;
    }

    // One row per call; the precision-specific readers are the only 12/16-bit
    // API variants (create/start/finish/destroy are shared). Calling the wrong
    // reader for the stream's precision error-exits — caught by the longjmp.
    while (cinfo.output_scanline < cinfo.output_height) {
        int y = (int)cinfo.output_scanline;
        if (prec <= 8) {
            JSAMPROW rp = (JSAMPROW)rowbuf;
            jpeg_read_scanlines(&cinfo, &rp, 1);
            JSAMPLE *row = (JSAMPLE *)rowbuf;
            for (int x = 0; x < w; x++)
                for (int c = 0; c < nc; c++)
                    out[(size_t)c * pixels + (size_t)y * w + x] = (int32_t)row[x * nc + c];
        } else if (prec <= 12) {
            J12SAMPROW rp = (J12SAMPROW)rowbuf;
            jpeg12_read_scanlines(&cinfo, &rp, 1);
            J12SAMPLE *row = (J12SAMPLE *)rowbuf;
            for (int x = 0; x < w; x++)
                for (int c = 0; c < nc; c++)
                    out[(size_t)c * pixels + (size_t)y * w + x] = (int32_t)row[x * nc + c];
        } else {
            J16SAMPROW rp = (J16SAMPROW)rowbuf;
            jpeg16_read_scanlines(&cinfo, &rp, 1);
            J16SAMPLE *row = (J16SAMPLE *)rowbuf;
            for (int x = 0; x < w; x++)
                for (int c = 0; c < nc; c++)
                    out[(size_t)c * pixels + (size_t)y * w + x] = (int32_t)row[x * nc + c];
        }
    }

    jpeg_finish_decompress(&cinfo);
    jpeg_destroy_decompress(&cinfo);
    free((void *)rowbuf);
    rowbuf = NULL;

    r->ok = 1;
    r->width = w;
    r->height = h;
    r->numcomps = nc;
    r->prec = prec;
    r->sgnd = 0; // T.81 samples are unsigned; DICOM signedness comes from PixelRepresentation
    r->samples = out;
}
*/
import "C"

import (
	"errors"
	"fmt"
	"image"
	"unsafe"
)

// jpegLosslessAvailable reports at compile time whether the libjpeg-turbo
// lossless decoder is linked in. The viewer checks it before letting a
// JPEG Lossless file past the unsupported-syntax veto.
const jpegLosslessAvailable = true

// decodeJPEGLossless decodes a JPEG Lossless (SOF3) stream into planar int32
// component samples plus geometry. samples holds numComps planes of
// width*height values (plane 0 first). signed is always false: T.81 samples
// are unsigned; DICOM signedness is carried by Pixel Representation instead.
func decodeJPEGLossless(data []byte) (width, height, numComps, prec int, signed bool, samples []int32, err error) {
	if len(data) == 0 {
		return 0, 0, 0, 0, false, nil, errors.New("empty JPEG Lossless data")
	}
	var res C.dq_jls_result
	C.dq_decode_jls((*C.uchar)(unsafe.Pointer(&data[0])), C.int(len(data)), &res)
	if res.ok == 0 {
		return 0, 0, 0, 0, false, nil, fmt.Errorf("jpeg lossless: %s", C.GoString(&res.err[0]))
	}
	defer C.dq_jls_free(res.samples)

	w, h, nc := int(res.width), int(res.height), int(res.numcomps)
	n := w * h * nc
	samples = make([]int32, n)
	src := unsafe.Slice((*int32)(unsafe.Pointer(res.samples)), n)
	copy(samples, src)
	return w, h, nc, int(res.prec), false, samples, nil
}

// decodeJPEGLosslessFrame decodes a JPEG Lossless frame into a decodedFrame,
// mapping monochrome samples through rescale slope/intercept into the
// windowing pipeline and colour samples into an RGB image. Unlike JPEG 2000,
// the codestream carries no signedness, so isSigned (DICOM Pixel
// Representation) drives two's-complement sign extension of monochrome
// samples before rescale — signed 16-bit CT/MR then windows correctly.
func decodeJPEGLosslessFrame(data []byte, slope, intercept float64, hasWindow bool, wc, ww float64, photometric string, isSigned bool) (*decodedFrame, error) {
	w, h, nc, prec, _, samples, err := decodeJPEGLossless(data)
	if err != nil {
		return nil, err
	}
	pixels := w * h
	if pixels <= 0 || len(samples) < pixels*nc {
		return nil, errors.New("jpeg lossless: decoded sample buffer too small")
	}
	if prec < 1 || prec > 16 {
		return nil, fmt.Errorf("jpeg lossless: unsupported precision %d", prec)
	}

	// Colour (3+ components): build an RGB image scaled to 8-bit per channel.
	if nc >= 3 {
		maxV := float64(int(1)<<uint(prec)) - 1
		if maxV <= 0 {
			maxV = 255
		}
		rp := samples[0:pixels]
		gp := samples[pixels : 2*pixels]
		bp := samples[2*pixels : 3*pixels]
		img := image.NewNRGBA(image.Rect(0, 0, w, h))
		for i := 0; i < pixels; i++ {
			img.Pix[i*4] = clampToUint8(float64(rp[i]) / maxV * 255)
			img.Pix[i*4+1] = clampToUint8(float64(gp[i]) / maxV * 255)
			img.Pix[i*4+2] = clampToUint8(float64(bp[i]) / maxV * 255)
			img.Pix[i*4+3] = 255
		}
		return &decodedFrame{rows: h, cols: w, colorImg: img}, nil
	}

	// Monochrome: sign-extend when the dataset declares signed pixels (SOF3
	// precision equals Bits Stored, so extension around 2^(prec-1) is exact),
	// then rescale into the float buffer the viewer windows.
	half := int64(1) << uint(prec-1)
	full := int64(1) << uint(prec)
	gray := make([]float32, pixels)
	for i := 0; i < pixels; i++ {
		v := int64(samples[i])
		if isSigned && v >= half {
			v -= full
		}
		gray[i] = float32(float64(v)*slope + intercept)
	}
	df := &decodedFrame{rows: h, cols: w, gray: gray, invert: photometric == "MONOCHROME1"}
	df.computeDefaultWindow(hasWindow, wc, ww)
	return df, nil
}
