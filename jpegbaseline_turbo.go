//go:build jpeglossless

// libjpeg-turbo decoding of 8-bit JPEG Baseline/Extended frames for display.
//
// The viewer decoded JPEG Baseline with Go's image/jpeg, which has no SIMD: an
// 800×600 echo frame took about 18 ms, then a second pass to turn its YCbCr
// planes into RGB. libjpeg-turbo is already linked for JPEG Lossless (same
// build tag), decodes several times faster, writes RGBA directly, and can
// decode at 1/2, 1/4 or 1/8 scale for a thumbnail — far less work than
// decoding in full and scaling down.
//
// Display only. The modification engine keeps Go's decoder (transcode.go's
// jpegFrameToNative): two JPEG decoders may differ by a level here and there on
// a lossy stream (upsampling and IDCT details), which is invisible on screen
// but must not change the pixels an export writes. Anything this cannot handle
// — 12-bit precision, CMYK, a stream libjpeg rejects — returns an error and the
// caller falls back to Go's decoder, so no file stops displaying.
package main

/*
#cgo CFLAGS: -IC:/msys64/mingw64/include
#cgo LDFLAGS: -LC:/msys64/mingw64/lib -l:libjpeg.a

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <setjmp.h>
#include <jpeglib.h>
#include <jerror.h>

typedef struct {
    int ok;
    int width;
    int height;
    int channels;          // 4 = RGBA, 1 = greyscale
    unsigned char *pixels; // malloc'd; free with dq_j8_free
    char err[256];
} dq_j8_result;

static void dq_j8_free(unsigned char *p) { free(p); }

typedef struct {
    struct jpeg_error_mgr pub;
    jmp_buf jb;
    char msg[JMSG_LENGTH_MAX];
} dq_j8_err_mgr;

static void dq_j8_error_exit(j_common_ptr cinfo) {
    dq_j8_err_mgr *e = (dq_j8_err_mgr *)cinfo->err;
    (*cinfo->err->format_message)(cinfo, e->msg);
    longjmp(e->jb, 1);
}

static void dq_j8_output_message(j_common_ptr cinfo) { (void)cinfo; }

static void dq_j8_fail(dq_j8_result *r, const char *msg) {
    r->ok = 0;
    strncpy(r->err, msg, sizeof(r->err) - 1);
    r->err[sizeof(r->err) - 1] = 0;
}

// dq_decode_j8 decodes one 8-bit JPEG stream to RGBA (3 components) or
// greyscale. max_side > 0 picks the smallest libjpeg scale (1/1..1/8) whose
// longer side is still at least max_side. Whole libjpeg lifecycle in this one
// frame, so the longjmp never crosses a Go stack frame (as dq_decode_jls).
static void dq_decode_j8(const unsigned char *data, int len, int max_side, dq_j8_result *r) {
    memset(r, 0, sizeof(*r));
    if (len < 4) { dq_j8_fail(r, "JPEG stream too short"); return; }

    struct jpeg_decompress_struct cinfo;
    dq_j8_err_mgr jerr;
    unsigned char * volatile out = NULL;

    cinfo.err = jpeg_std_error(&jerr.pub);
    jerr.pub.error_exit = dq_j8_error_exit;
    jerr.pub.output_message = dq_j8_output_message;
    jerr.msg[0] = 0;
    if (setjmp(jerr.jb)) {
        free((void *)out);
        jpeg_destroy_decompress(&cinfo);
        dq_j8_fail(r, jerr.msg[0] ? jerr.msg : "JPEG decode failed");
        return;
    }

    jpeg_create_decompress(&cinfo);
    jpeg_mem_src(&cinfo, data, (unsigned long)len);
    jpeg_read_header(&cinfo, TRUE);

    if (cinfo.data_precision != 8) {
        jpeg_destroy_decompress(&cinfo);
        dq_j8_fail(r, "not an 8-bit JPEG");
        return;
    }
    int channels;
    if (cinfo.num_components == 3) {
        cinfo.out_color_space = JCS_EXT_RGBA; // alpha written as 0xFF
        channels = 4;
    } else if (cinfo.num_components == 1) {
        cinfo.out_color_space = JCS_GRAYSCALE;
        channels = 1;
    } else {
        jpeg_destroy_decompress(&cinfo);
        dq_j8_fail(r, "unsupported JPEG component count");
        return;
    }

    if (max_side > 0) {
        unsigned int side = cinfo.image_width > cinfo.image_height ? cinfo.image_width : cinfo.image_height;
        unsigned int denom = 1;
        while (denom < 8 && (side + denom * 2 - 1) / (denom * 2) >= (unsigned int)max_side) {
            denom *= 2;
        }
        cinfo.scale_num = 1;
        cinfo.scale_denom = denom;
    }

    jpeg_start_decompress(&cinfo);
    int w = (int)cinfo.output_width;
    int h = (int)cinfo.output_height;
    if (w <= 0 || h <= 0 || (size_t)w * (size_t)h > ((size_t)1 << 28)) {
        jpeg_destroy_decompress(&cinfo);
        dq_j8_fail(r, "unsupported JPEG dimensions");
        return;
    }
    size_t stride = (size_t)w * (size_t)channels;
    out = (unsigned char *)malloc(stride * (size_t)h);
    if (out == NULL) {
        jpeg_destroy_decompress(&cinfo);
        dq_j8_fail(r, "out of memory");
        return;
    }
    while (cinfo.output_scanline < cinfo.output_height) {
        JSAMPROW row = out + stride * (size_t)cinfo.output_scanline;
        jpeg_read_scanlines(&cinfo, &row, 1);
    }
    jpeg_finish_decompress(&cinfo);
    jpeg_destroy_decompress(&cinfo);

    r->ok = 1;
    r->width = w;
    r->height = h;
    r->channels = channels;
    r->pixels = out;
}
*/
import "C"

import (
	"errors"
	"fmt"
	"image"
	"unsafe"
)

// decodeJPEGForDisplay decodes an 8-bit JPEG Baseline/Extended frame for the
// viewer: *image.RGBA for colour, *image.Gray for greyscale — the same image
// kinds Go's decoder yields to the display path, so nothing downstream
// changes. maxSide > 0 decodes at a reduced scale no smaller than maxSide.
func decodeJPEGForDisplay(data []byte, maxSide int) (image.Image, error) {
	if len(data) == 0 {
		return nil, errors.New("empty JPEG data")
	}
	var res C.dq_j8_result
	C.dq_decode_j8((*C.uchar)(unsafe.Pointer(&data[0])), C.int(len(data)), C.int(maxSide), &res)
	if res.ok == 0 {
		return nil, fmt.Errorf("jpeg: %s", C.GoString(&res.err[0]))
	}
	defer C.dq_j8_free(res.pixels)
	w, h, ch := int(res.width), int(res.height), int(res.channels)
	src := unsafe.Slice((*byte)(unsafe.Pointer(res.pixels)), w*h*ch)
	if ch == 4 {
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		copy(img.Pix, src)
		return img, nil
	}
	img := image.NewGray(image.Rect(0, 0, w, h))
	copy(img.Pix, src)
	return img, nil
}
