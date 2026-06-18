//go:build openjpeg

// Package-level JPEG 2000 decoder backed by OpenJPEG (libopenjp2), linked
// statically. Built only when the "openjpeg" build tag is set; otherwise
// jpeg2000_stub.go provides a stub that reports the format as unsupported.
//
// Requires the MSYS2 package: pacman -S mingw-w64-x86_64-openjpeg2
package main

/*
#cgo CFLAGS: -IC:/msys64/mingw64/include/openjpeg-2.5 -DOPJ_STATIC
#cgo LDFLAGS: -LC:/msys64/mingw64/lib -l:libopenjp2.a -lm

#include <stdlib.h>
#include <string.h>
#include <openjpeg.h>

typedef struct {
    const unsigned char *data;
    OPJ_SIZE_T size;
    OPJ_SIZE_T off;
} dq_mem;

static OPJ_SIZE_T dq_read(void *buf, OPJ_SIZE_T n, void *user) {
    dq_mem *m = (dq_mem *)user;
    OPJ_SIZE_T rem = m->size - m->off;
    if (rem == 0) return (OPJ_SIZE_T)-1; // EOF
    if (n > rem) n = rem;
    memcpy(buf, m->data + m->off, n);
    m->off += n;
    return n;
}

static OPJ_OFF_T dq_skip(OPJ_OFF_T n, void *user) {
    dq_mem *m = (dq_mem *)user;
    if (n < 0) return -1;
    OPJ_SIZE_T rem = m->size - m->off;
    OPJ_SIZE_T adv = (OPJ_SIZE_T)n;
    if (adv > rem) adv = rem;
    m->off += adv;
    return (OPJ_OFF_T)adv;
}

static OPJ_BOOL dq_seek(OPJ_OFF_T n, void *user) {
    dq_mem *m = (dq_mem *)user;
    if (n < 0 || (OPJ_SIZE_T)n > m->size) return OPJ_FALSE;
    m->off = (OPJ_SIZE_T)n;
    return OPJ_TRUE;
}

typedef struct {
    int ok;
    int width;
    int height;
    int numcomps;
    int prec;
    int sgnd;
    int32_t *samples; // planar: numcomps planes of width*height, malloc'd; free with dq_free
    char err[256];
} dq_j2k_result;

static void dq_err(dq_j2k_result *r, const char *msg) {
    r->ok = 0;
    strncpy(r->err, msg, sizeof(r->err) - 1);
    r->err[sizeof(r->err) - 1] = 0;
}

static void dq_free(int32_t *p) { free(p); }

// Quietly swallow OpenJPEG's diagnostic messages.
static void dq_quiet(const char *msg, void *client) { (void)msg; (void)client; }

static void dq_decode_j2k(const unsigned char *data, int len, dq_j2k_result *r) {
    memset(r, 0, sizeof(*r));
    if (len < 12) { dq_err(r, "JPEG 2000 codestream too short"); return; }

    // DICOM encapsulates the raw J2K codestream (SOC marker FF 4F); some files
    // carry the JP2 box format instead (signature 00 00 00 0C 6A 50).
    OPJ_CODEC_FORMAT fmt = OPJ_CODEC_J2K;
    if (data[0] == 0x00 && data[1] == 0x00 && data[2] == 0x00 && data[3] == 0x0C &&
        data[4] == 0x6A && data[5] == 0x50) {
        fmt = OPJ_CODEC_JP2;
    }

    dq_mem mem;
    mem.data = data;
    mem.size = (OPJ_SIZE_T)len;
    mem.off = 0;

    // OPJ_TRUE = input (read) stream.
    opj_stream_t *stream = opj_stream_default_create(OPJ_TRUE);
    if (!stream) { dq_err(r, "opj_stream_default_create failed"); return; }
    opj_stream_set_user_data(stream, &mem, NULL);
    opj_stream_set_user_data_length(stream, mem.size);
    opj_stream_set_read_function(stream, dq_read);
    opj_stream_set_skip_function(stream, dq_skip);
    opj_stream_set_seek_function(stream, dq_seek);

    opj_codec_t *codec = opj_create_decompress(fmt);
    if (!codec) {
        opj_stream_destroy(stream);
        dq_err(r, "opj_create_decompress failed");
        return;
    }
    opj_set_info_handler(codec, dq_quiet, NULL);
    opj_set_warning_handler(codec, dq_quiet, NULL);
    opj_set_error_handler(codec, dq_quiet, NULL);

    opj_dparameters_t params;
    opj_set_default_decoder_parameters(&params);
    if (!opj_setup_decoder(codec, &params)) {
        opj_destroy_codec(codec);
        opj_stream_destroy(stream);
        dq_err(r, "opj_setup_decoder failed");
        return;
    }

    opj_image_t *image = NULL;
    if (!opj_read_header(stream, codec, &image)) {
        if (image) opj_image_destroy(image);
        opj_destroy_codec(codec);
        opj_stream_destroy(stream);
        dq_err(r, "opj_read_header failed");
        return;
    }
    if (!opj_decode(codec, stream, image) || !opj_end_decompress(codec, stream)) {
        opj_image_destroy(image);
        opj_destroy_codec(codec);
        opj_stream_destroy(stream);
        dq_err(r, "opj_decode failed");
        return;
    }

    OPJ_UINT32 nc = image->numcomps;
    if (nc < 1 || image->comps == NULL) {
        opj_image_destroy(image);
        opj_destroy_codec(codec);
        opj_stream_destroy(stream);
        dq_err(r, "no image components");
        return;
    }
    OPJ_UINT32 w = image->comps[0].w;
    OPJ_UINT32 h = image->comps[0].h;
    for (OPJ_UINT32 c = 0; c < nc; c++) {
        if (image->comps[c].w != w || image->comps[c].h != h || image->comps[c].data == NULL) {
            opj_image_destroy(image);
            opj_destroy_codec(codec);
            opj_stream_destroy(stream);
            dq_err(r, "unsupported component geometry (subsampled or empty)");
            return;
        }
    }

    size_t pixels = (size_t)w * (size_t)h;
    int32_t *out = (int32_t *)malloc(pixels * nc * sizeof(int32_t));
    if (!out) {
        opj_image_destroy(image);
        opj_destroy_codec(codec);
        opj_stream_destroy(stream);
        dq_err(r, "out of memory");
        return;
    }
    for (OPJ_UINT32 c = 0; c < nc; c++) {
        memcpy(out + (size_t)c * pixels, image->comps[c].data, pixels * sizeof(int32_t));
    }

    r->ok = 1;
    r->width = (int)w;
    r->height = (int)h;
    r->numcomps = (int)nc;
    r->prec = (int)image->comps[0].prec;
    r->sgnd = (int)image->comps[0].sgnd;
    r->samples = out;

    opj_image_destroy(image);
    opj_destroy_codec(codec);
    opj_stream_destroy(stream);
}
*/
import "C"

import (
	"errors"
	"fmt"
	"image"
	"unsafe"
)

// decodeJPEG2000 decodes a JPEG 2000 codestream (or JP2) into planar int32
// component samples plus geometry. samples holds numComps planes of
// width*height values (plane 0 first).
func decodeJPEG2000(data []byte) (width, height, numComps, prec int, signed bool, samples []int32, err error) {
	if len(data) == 0 {
		return 0, 0, 0, 0, false, nil, errors.New("empty JPEG 2000 data")
	}
	var res C.dq_j2k_result
	C.dq_decode_j2k((*C.uchar)(unsafe.Pointer(&data[0])), C.int(len(data)), &res)
	if res.ok == 0 {
		return 0, 0, 0, 0, false, nil, fmt.Errorf("jpeg2000: %s", C.GoString(&res.err[0]))
	}
	defer C.dq_free(res.samples)

	w, h, nc := int(res.width), int(res.height), int(res.numcomps)
	n := w * h * nc
	samples = make([]int32, n)
	src := unsafe.Slice((*int32)(unsafe.Pointer(res.samples)), n)
	copy(samples, src)
	return w, h, nc, int(res.prec), res.sgnd != 0, samples, nil
}

// decodeJPEG2000Frame decodes a JPEG 2000 frame into a decodedFrame, mapping
// monochrome samples through rescale slope/intercept into the windowing pipeline
// (so window/level and colour maps apply) and colour samples into an RGB image.
func decodeJPEG2000Frame(data []byte, slope, intercept float64, hasWindow bool, wc, ww float64, photometric string) (*decodedFrame, error) {
	w, h, nc, prec, _, samples, err := decodeJPEG2000(data)
	if err != nil {
		return nil, err
	}
	pixels := w * h
	if pixels <= 0 || len(samples) < pixels*nc {
		return nil, errors.New("jpeg2000: decoded sample buffer too small")
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

	// Monochrome: rescale into the float buffer the viewer windows.
	gray := make([]float32, pixels)
	for i := 0; i < pixels; i++ {
		gray[i] = float32(float64(samples[i])*slope + intercept)
	}
	df := &decodedFrame{rows: h, cols: w, gray: gray, invert: photometric == "MONOCHROME1"}
	df.computeDefaultWindow(hasWindow, wc, ww)
	return df, nil
}
