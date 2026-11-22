package main

/*
#cgo pkg-config: x264 opus
#include <stdlib.h>
#include <string.h>
#include <stdint.h>
#include <x264.h>
#include <opus/opus.h>

typedef struct { x264_t *h; x264_picture_t pic; int w, h_; int64_t pts; } venc;

static venc *venc_open(int w, int hgt, int fps, int kbps, int keyint) {
    venc *e = calloc(1, sizeof *e);
    x264_param_t p;
    if (x264_param_default_preset(&p, "ultrafast", "zerolatency") < 0) { free(e); return 0; }
    p.i_csp = X264_CSP_I420; p.i_width = w; p.i_height = hgt;
    p.i_fps_num = fps; p.i_fps_den = 1; p.i_timebase_num = 1; p.i_timebase_den = fps;
    p.b_repeat_headers = 1; p.b_annexb = 1; p.b_aud = 0;
    p.i_bframe = 0; p.i_keyint_max = keyint; p.i_keyint_min = keyint; p.i_scenecut_threshold = 0;
    p.rc.i_rc_method = X264_RC_ABR; p.rc.i_bitrate = kbps; p.rc.i_vbv_max_bitrate = kbps; p.rc.i_vbv_buffer_size = kbps / fps * 3;
    p.i_level_idc = 31;
    if (x264_param_apply_profile(&p, "baseline") < 0) { free(e); return 0; }
    e->h = x264_encoder_open(&p);
    if (!e->h) { free(e); return 0; }
    x264_picture_alloc(&e->pic, X264_CSP_I420, w, hgt);
    e->w = w; e->h_ = hgt;
    return e;
}
// encodes one I420 frame; returns the access unit size (0 = nothing, <0 error); out points to x264-owned memory
static int venc_encode(venc *e, const uint8_t *yuv, int force_idr, uint8_t **out) {
    size_t ys = (size_t)e->w * e->h_, cs = ys / 4;
    memcpy(e->pic.img.plane[0], yuv, ys); memcpy(e->pic.img.plane[1], yuv + ys, cs); memcpy(e->pic.img.plane[2], yuv + ys + cs, cs);
    e->pic.i_pts = e->pts++; e->pic.i_type = force_idr ? X264_TYPE_IDR : X264_TYPE_AUTO;
    x264_nal_t *nals; int n; x264_picture_t po;
    int sz = x264_encoder_encode(e->h, &nals, &n, &e->pic, &po);
    if (sz <= 0) return sz;
    *out = nals[0].p_payload;
    return sz;
}
static void venc_close(venc *e) { if (!e) return; x264_encoder_close(e->h); x264_picture_clean(&e->pic); free(e); }

static OpusEncoder *aenc_open(int kbps) {
    int err; OpusEncoder *e = opus_encoder_create(48000, 2, OPUS_APPLICATION_RESTRICTED_LOWDELAY, &err);
    if (!e) return 0;
    opus_encoder_ctl(e, OPUS_SET_BITRATE(kbps * 1000));
    return e;
}
static int aenc_encode(OpusEncoder *e, const int16_t *pcm, int frames, uint8_t *out, int cap) { return opus_encode(e, pcm, frames, out, cap); }
*/
import "C"

import (
	"errors"
	"unsafe"
)

// videoEncoder is x264 (Baseline, no B-frames, zerolatency) used through its C API: one frame in, one access unit out,
// with no pipe and no lookahead (a CLI such as ffmpeg would add a pipe hop and AU framing delay).
type videoEncoder struct {
	e     *C.venc
	w, h  int
	frame int
}

func newVideoEncoder(w, h, fps, kbps, keyint int) (*videoEncoder, error) {
	e := C.venc_open(C.int(w), C.int(h), C.int(fps), C.int(kbps), C.int(keyint))
	if e == nil {
		return nil, errors.New("x264 open failed")
	}
	return &videoEncoder{e: e, w: w, h: h}, nil
}

// Encode returns an Annex-B access unit (copied). forceIDR asks for a keyframe on this frame.
func (v *videoEncoder) Encode(i420 []byte, forceIDR bool) ([]byte, error) {
	if len(i420) != v.w*v.h*3/2 {
		return nil, errors.New("bad frame size")
	}
	var out *C.uint8_t
	f := 0
	if forceIDR {
		f = 1
	}
	n := C.venc_encode(v.e, (*C.uint8_t)(unsafe.Pointer(&i420[0])), C.int(f), &out)
	if n < 0 {
		return nil, errors.New("x264 encode failed")
	}
	if n == 0 {
		return nil, nil
	}
	return C.GoBytes(unsafe.Pointer(out), n), nil
}

func (v *videoEncoder) Close() { C.venc_close(v.e); v.e = nil }

// audioEncoder is Opus (restricted low delay, 20 ms frames, 48 kHz stereo).
type audioEncoder struct{ e *C.OpusEncoder }

func newAudioEncoder(kbps int) (*audioEncoder, error) {
	e := C.aenc_open(C.int(kbps))
	if e == nil {
		return nil, errors.New("opus open failed")
	}
	return &audioEncoder{e: e}, nil
}

// Encode takes 960 interleaved stereo frames (20 ms).
func (a *audioEncoder) Encode(pcm []int16) ([]byte, error) {
	if len(pcm) != 960*2 {
		return nil, errors.New("bad audio size")
	}
	buf := make([]byte, 1275)
	n := C.aenc_encode(a.e, (*C.int16_t)(unsafe.Pointer(&pcm[0])), 960, (*C.uint8_t)(unsafe.Pointer(&buf[0])), C.int(len(buf)))
	if n < 0 {
		return nil, errors.New("opus encode failed")
	}
	return buf[:n], nil
}

func (a *audioEncoder) Close() { C.opus_encoder_destroy(a.e); a.e = nil }
