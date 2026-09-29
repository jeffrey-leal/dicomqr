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

// dq_reduce_for picks how many resolution levels to discard so the decoded
// image is still at least max_side on its longer side: each level halves both
// dimensions (rounding up), and a codestream with N resolutions can drop at
// most N-1. max_side <= 0 means full resolution.
static OPJ_UINT32 dq_reduce_for(opj_codec_t *codec, opj_image_t *image, int max_side) {
    if (max_side <= 0) return 0;
    opj_codestream_info_v2_t *info = opj_get_cstr_info(codec);
    if (!info) return 0;
    OPJ_UINT32 numres = 0;
    if (info->m_default_tile_info.tccp_info) {
        numres = info->m_default_tile_info.tccp_info[0].numresolutions;
        for (OPJ_UINT32 c = 1; c < info->nbcomps; c++) {
            OPJ_UINT32 n = info->m_default_tile_info.tccp_info[c].numresolutions;
            if (n < numres) numres = n;
        }
    }
    opj_destroy_cstr_info(&info);
    if (numres <= 1) return 0;
    OPJ_UINT32 w = image->x1 - image->x0, h = image->y1 - image->y0;
    OPJ_UINT32 side = w > h ? w : h;
    OPJ_UINT32 reduce = 0;
    while (reduce + 1 < numres) {
        OPJ_UINT32 next = (side + (1u << (reduce + 1)) - 1) >> (reduce + 1);
        if ((int)next < max_side) break;
        reduce++;
    }
    return reduce;
}

// dq_decode_j2k decodes one codestream. threads > 1 decodes code-blocks on that
// many worker threads (when the library was built with thread support);
// max_side > 0 decodes at the lowest resolution level still at least that
// large, for a thumbnail.
static void dq_decode_j2k(const unsigned char *data, int len, int threads, int max_side, dq_j2k_result *r) {
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
    // Thread count has to be set between setup and reading the header. A
    // library built without thread support decodes on the calling thread, as
    // before, so failure here is not an error.
    if (threads > 1 && opj_has_thread_support()) {
        opj_codec_set_threads(codec, threads);
    }

    opj_image_t *image = NULL;
    if (!opj_read_header(stream, codec, &image)) {
        if (image) opj_image_destroy(image);
        opj_destroy_codec(codec);
        opj_stream_destroy(stream);
        dq_err(r, "opj_read_header failed");
        return;
    }
    // A factor the codec refuses leaves it at full resolution, which still
    // makes a thumbnail — so its result is deliberately not treated as fatal.
    OPJ_UINT32 reduce = dq_reduce_for(codec, image, max_side);
    if (reduce > 0) {
        opj_set_decoded_resolution_factor(codec, reduce);
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

// ---- encoding (reversible / lossless only) ----

// Growable memory sink for OpenJPEG's write stream. The encoder seeks back to
// patch earlier bytes (e.g. tile-part lengths), so len tracks the high-water
// mark independently of the current offset, and growth is zero-filled so a
// seek past the end reads deterministic bytes.
typedef struct {
    unsigned char *data;
    OPJ_SIZE_T cap;
    OPJ_SIZE_T len;
    OPJ_SIZE_T off;
} dq_wmem;

static int dq_wmem_reserve(dq_wmem *m, OPJ_SIZE_T need) {
    if (need <= m->cap) return 1;
    OPJ_SIZE_T cap = m->cap ? m->cap : 65536;
    while (cap < need) cap *= 2;
    unsigned char *p = (unsigned char *)realloc(m->data, cap);
    if (!p) return 0;
    memset(p + m->cap, 0, cap - m->cap);
    m->data = p;
    m->cap = cap;
    return 1;
}

static OPJ_SIZE_T dq_wwrite(void *buf, OPJ_SIZE_T n, void *user) {
    dq_wmem *m = (dq_wmem *)user;
    if (!dq_wmem_reserve(m, m->off + n)) return (OPJ_SIZE_T)-1;
    memcpy(m->data + m->off, buf, n);
    m->off += n;
    if (m->off > m->len) m->len = m->off;
    return n;
}

static OPJ_OFF_T dq_wskip(OPJ_OFF_T n, void *user) {
    dq_wmem *m = (dq_wmem *)user;
    if (n < 0) return -1;
    if (!dq_wmem_reserve(m, m->off + (OPJ_SIZE_T)n)) return -1;
    m->off += (OPJ_SIZE_T)n;
    if (m->off > m->len) m->len = m->off;
    return n;
}

static OPJ_BOOL dq_wseek(OPJ_OFF_T n, void *user) {
    dq_wmem *m = (dq_wmem *)user;
    if (n < 0) return OPJ_FALSE;
    if (!dq_wmem_reserve(m, (OPJ_SIZE_T)n)) return OPJ_FALSE;
    m->off = (OPJ_SIZE_T)n;
    if (m->off > m->len) m->len = m->off;
    return OPJ_TRUE;
}

typedef struct {
    int ok;
    unsigned char *data; // malloc'd; free with dq_free_bytes
    OPJ_SIZE_T size;
    char err[256];
} dq_j2k_enc_result;

static void dq_enc_err(dq_j2k_enc_result *r, const char *msg) {
    r->ok = 0;
    strncpy(r->err, msg, sizeof(r->err) - 1);
    r->err[sizeof(r->err) - 1] = 0;
}

static void dq_free_bytes(unsigned char *p) { free(p); }

// dq_encode_j2k encodes planar int32 samples (nc planes of w*h, plane 0 first
// — the exact layout dq_decode_j2k produces) as a reversible (5/3 wavelet,
// lossless) raw J2K codestream. mct applies the reversible colour transform
// and is only legal for 3 components.
static void dq_encode_j2k(const int32_t *samples, int w, int h, int nc,
                          int prec, int sgnd, int mct, dq_j2k_enc_result *r) {
    memset(r, 0, sizeof(*r));
    if (w <= 0 || h <= 0 || nc < 1 || nc > 4 || prec < 1 || prec > 16) {
        dq_enc_err(r, "unsupported image geometry");
        return;
    }

    opj_image_cmptparm_t cmpt[4];
    memset(cmpt, 0, sizeof(cmpt));
    for (int c = 0; c < nc; c++) {
        cmpt[c].dx = 1;
        cmpt[c].dy = 1;
        cmpt[c].w = (OPJ_UINT32)w;
        cmpt[c].h = (OPJ_UINT32)h;
        cmpt[c].x0 = 0;
        cmpt[c].y0 = 0;
        cmpt[c].prec = (OPJ_UINT32)prec;
        cmpt[c].sgnd = sgnd ? 1 : 0;
    }
    opj_image_t *image = opj_image_create((OPJ_UINT32)nc, cmpt,
                                          nc >= 3 ? OPJ_CLRSPC_SRGB : OPJ_CLRSPC_GRAY);
    if (!image) { dq_enc_err(r, "opj_image_create failed"); return; }
    image->x0 = 0;
    image->y0 = 0;
    image->x1 = (OPJ_UINT32)w;
    image->y1 = (OPJ_UINT32)h;
    size_t pixels = (size_t)w * (size_t)h;
    for (int c = 0; c < nc; c++) {
        memcpy(image->comps[c].data, samples + (size_t)c * pixels, pixels * sizeof(int32_t));
    }

    opj_cparameters_t params;
    opj_set_default_encoder_parameters(&params);
    // Reversible: one layer, rate 0 (no truncation), 5/3 wavelet. These are
    // the opj_compress defaults for lossless, spelled out rather than assumed.
    params.tcp_numlayers = 1;
    params.tcp_rates[0] = 0;
    params.cp_disto_alloc = 1;
    params.irreversible = 0;
    params.tcp_mct = (mct && nc == 3) ? 1 : 0;
    // The default 6 resolution levels are invalid for images smaller than
    // 2^5 in either dimension (opj_setup_encoder rejects them); scale down.
    int mind = w < h ? w : h;
    int nres = 1;
    while (nres < 6 && (1 << nres) <= mind) nres++;
    params.numresolution = nres;

    opj_codec_t *codec = opj_create_compress(OPJ_CODEC_J2K);
    if (!codec) {
        opj_image_destroy(image);
        dq_enc_err(r, "opj_create_compress failed");
        return;
    }
    opj_set_info_handler(codec, dq_quiet, NULL);
    opj_set_warning_handler(codec, dq_quiet, NULL);
    opj_set_error_handler(codec, dq_quiet, NULL);

    if (!opj_setup_encoder(codec, &params, image)) {
        opj_destroy_codec(codec);
        opj_image_destroy(image);
        dq_enc_err(r, "opj_setup_encoder failed");
        return;
    }

    dq_wmem mem;
    memset(&mem, 0, sizeof(mem));
    // OPJ_FALSE = output (write) stream.
    opj_stream_t *stream = opj_stream_default_create(OPJ_FALSE);
    if (!stream) {
        opj_destroy_codec(codec);
        opj_image_destroy(image);
        dq_enc_err(r, "opj_stream_default_create failed");
        return;
    }
    opj_stream_set_user_data(stream, &mem, NULL);
    opj_stream_set_write_function(stream, dq_wwrite);
    opj_stream_set_skip_function(stream, dq_wskip);
    opj_stream_set_seek_function(stream, dq_wseek);

    if (!opj_start_compress(codec, image, stream) ||
        !opj_encode(codec, stream) ||
        !opj_end_compress(codec, stream)) {
        free(mem.data);
        opj_stream_destroy(stream);
        opj_destroy_codec(codec);
        opj_image_destroy(image);
        dq_enc_err(r, "opj_encode failed");
        return;
    }

    opj_stream_destroy(stream);
    opj_destroy_codec(codec);
    opj_image_destroy(image);

    r->ok = 1;
    r->data = mem.data;
    r->size = mem.len;
}
*/
import "C"

import (
	"errors"
	"fmt"
	"image"
	"unsafe"
)

// jpeg2000Available reports at compile time whether the OpenJPEG decoder is
// linked in. The local decompress fallback checks it before attempting to
// transcode a JPEG 2000 file.
const jpeg2000Available = true

// jpeg2000ThreadSupport reports whether the linked OpenJPEG can decode on
// several threads; without it frameDecodeOpts.threads is silently ignored.
func jpeg2000ThreadSupport() bool { return C.opj_has_thread_support() != 0 }

// decodeJPEG2000 decodes a JPEG 2000 codestream (or JP2) into planar int32
// component samples plus geometry. samples holds numComps planes of
// width*height values (plane 0 first). Single-threaded, full resolution: what
// every conversion and verification path needs.
func decodeJPEG2000(data []byte) (width, height, numComps, prec int, signed bool, samples []int32, err error) {
	return decodeJPEG2000Opts(data, frameDecodeOpts{})
}

// decodeJPEG2000Opts is decodeJPEG2000 tuned for where the image is going: see
// frameDecodeOpts. With maxSide set, width and height are the reduced size.
func decodeJPEG2000Opts(data []byte, opts frameDecodeOpts) (width, height, numComps, prec int, signed bool, samples []int32, err error) {
	if len(data) == 0 {
		return 0, 0, 0, 0, false, nil, errors.New("empty JPEG 2000 data")
	}
	var res C.dq_j2k_result
	C.dq_decode_j2k((*C.uchar)(unsafe.Pointer(&data[0])), C.int(len(data)),
		C.int(opts.threads), C.int(opts.maxSide), &res)
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
func decodeJPEG2000Frame(data []byte, opts frameDecodeOpts, slope, intercept float64, hasWindow bool, wc, ww float64, photometric string) (*decodedFrame, error) {
	w, h, nc, prec, _, samples, err := decodeJPEG2000Opts(data, opts)
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

	// Monochrome: the stored values the viewer windows (see grayframe.go).
	df := newGrayFrame(h, w, samples[:pixels], slope, intercept, photometric == "MONOCHROME1")
	df.computeDefaultWindow(hasWindow, wc, ww)
	return df, nil
}

// encodeJPEG2000Lossless encodes planar int32 samples (numComps planes of
// width*height, plane 0 first — the same layout decodeJPEG2000 returns) as a
// reversible JPEG 2000 codestream, suitable for encapsulation under the JPEG
// 2000 Lossless transfer syntax. signed samples must arrive sign-extended
// (real negative int32 values); mct applies the reversible colour transform
// and is honoured only for 3-component input.
func encodeJPEG2000Lossless(samples []int32, width, height, numComps, prec int, signed, mct bool) ([]byte, error) {
	if width <= 0 || height <= 0 || numComps < 1 || len(samples) < width*height*numComps {
		return nil, errors.New("jpeg2000 encode: sample buffer smaller than geometry")
	}
	cSigned, cMCT := C.int(0), C.int(0)
	if signed {
		cSigned = 1
	}
	if mct {
		cMCT = 1
	}
	var res C.dq_j2k_enc_result
	C.dq_encode_j2k((*C.int32_t)(unsafe.Pointer(&samples[0])),
		C.int(width), C.int(height), C.int(numComps), C.int(prec), cSigned, cMCT, &res)
	if res.ok == 0 {
		return nil, fmt.Errorf("jpeg2000 encode: %s", C.GoString(&res.err[0]))
	}
	defer C.dq_free_bytes(res.data)
	return C.GoBytes(unsafe.Pointer(res.data), C.int(res.size)), nil
}
