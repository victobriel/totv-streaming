// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 victobriel

package main

/*
#cgo pkg-config: x264 opus
#include <stdlib.h>
#include <string.h>
#include <stdint.h>
#include <x264.h>
#include <opus/opus.h>

typedef struct { x264_t *h; x264_picture_t pic; int w, h_; int64_t pts; } venc;

static venc *venc_open(int w, int hgt, int fps_num, int fps_den, int crf, int maxkbps, int bufkbit, int keyint, int threads, int level, const char *preset) {
    venc *e = calloc(1, sizeof *e);
    x264_param_t p;
    if (x264_param_default_preset(&p, preset, "zerolatency") < 0) { free(e); return 0; }
    p.i_csp = X264_CSP_I420; p.i_width = w; p.i_height = hgt;
    p.i_fps_num = fps_num; p.i_fps_den = fps_den; p.i_timebase_num = fps_den; p.i_timebase_den = fps_num;
    p.b_repeat_headers = 1; p.b_annexb = 1; p.b_aud = 0;
    p.i_bframe = 0; p.i_keyint_max = keyint; p.i_keyint_min = keyint; p.i_scenecut_threshold = 0;
    p.i_threads = threads; p.b_sliced_threads = 1; p.i_lookahead_threads = 0; p.i_sync_lookahead = 0; p.rc.i_lookahead = 0;
    p.b_deblocking_filter = 1; p.i_deblocking_filter_alphac0 = 0; p.i_deblocking_filter_beta = 0;
    // constant quality, capped: CRF decides the quality, VBV (maxrate / buffer) bounds the bursts on the LAN
    p.rc.i_rc_method = X264_RC_CRF; p.rc.f_rf_constant = (float)crf;
    p.rc.i_vbv_max_bitrate = maxkbps; p.rc.i_vbv_buffer_size = bufkbit;
    p.i_level_idc = level;
    p.b_vfr_input = 0;
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
	"fmt"
	"math"
	"regexp"
	"unsafe"
)

// videoEncoder is x264 (Baseline, no B-frames, zerolatency, CRF with a VBV cap) used through its C API: one frame in, one access unit out,
// with no pipe and no lookahead (a CLI such as ffmpeg would add a pipe hop and AU framing delay).
type videoEncoder struct {
	e     *C.venc
	w, h  int
	frame int
}

// encoderParams: what newVideoEncoder needs. FPS is the core's real rate (50 for a PAL N64, 59.94 for NTSC), MaxKbps the VBV ceiling.
type encoderParams struct {
	W, H      int
	FPS       float64
	CRF       int
	MaxKbps   int
	BufFrames int    // VBV buffer, in frames at MaxKbps (>= 6)
	Keyint    int    // IDR distance in frames (0 = one second)
	Threads   int    // x264 slices/threads (0 = 4)
	Preset    string // x264 preset ("" = superfast); Baseline and zerolatency are always applied
}

// fpsRational gives the exact frame rate x264 should assume: NTSC 59.94 and 29.97 as 60000/1001 and 30000/1001, everything else rounded to an integer.
func fpsRational(f float64) (int, int) {
	if f < 10 || f > 144 {
		return 60, 1
	}
	if r := f * 1001 / 1000; math.Abs(r-math.Round(r)) < 0.01 && math.Abs(f-math.Round(f)) > 0.02 { // 59.94 -> 60000/1001
		return int(math.Round(r)) * 1000, 1001
	}
	return int(math.Round(f)), 1
}

// h264Level is the smallest H.264 level (level_idc) whose macroblock rate and frame size hold w x h at fps; Baseline limits only.
func h264Level(w, h int, fps float64) int {
	mbs := ((w + 15) / 16) * ((h + 15) / 16)
	rate := float64(mbs) * math.Max(fps, 1)
	for _, l := range []struct {
		idc, fs int
		mbps    float64
	}{{31, 3600, 108000}, {32, 5120, 216000}, {40, 8192, 245760}, {42, 8704, 522240}, {50, 22080, 589824}} {
		if mbs <= l.fs && rate <= l.mbps {
			return l.idc
		}
	}
	return 51
}

// h264FmtpFor is the SDP fmtp of the stream: Constrained Baseline (42e0) at the level the picture really needs. level-asymmetry-allowed lets the
// receiver decode a level different from the one it offered (the level part of profile-level-id is not matched by the negotiation).
func h264FmtpFor(level int) string {
	return fmt.Sprintf("level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e0%02x", level)
}

var fmtpLevelRe = regexp.MustCompile(`(?m)^(a=fmtp:\d+ [^\r\n]*profile-level-id=42e0)[0-9a-fA-F]{2}`)

// announceLevel sets the level of the answer's Constrained Baseline H.264 line to the real one: pion answers with the level the browser OFFERED (Chrome offers 3.1,
// 42e01f), which is below what a 1280x720 stream at 50/60 fps needs (3.2). With level-asymmetry-allowed=1 on both sides the receiver may decode another level.
func announceLevel(sdp string, level int) string {
	return fmtpLevelRe.ReplaceAllString(sdp, fmt.Sprintf("${1}%02x", level))
}

func newVideoEncoder(p encoderParams) (*videoEncoder, error) {
	if p.Keyint <= 0 {
		p.Keyint = int(math.Round(p.FPS))
	}
	if p.Threads <= 0 {
		p.Threads = 4
	}
	if p.BufFrames < 6 {
		p.BufFrames = 6
	}
	if p.Preset == "" || !presetOK(p.Preset) {
		p.Preset = "superfast"
	}
	if p.CRF < 10 || p.CRF > 40 {
		p.CRF = 22
	}
	num, den := fpsRational(p.FPS)
	fps := float64(num) / float64(den)
	buf := int(float64(p.MaxKbps) * float64(p.BufFrames) / fps)
	cp := C.CString(p.Preset)
	defer C.free(unsafe.Pointer(cp))
	e := C.venc_open(C.int(p.W), C.int(p.H), C.int(num), C.int(den), C.int(p.CRF), C.int(p.MaxKbps), C.int(buf), C.int(p.Keyint), C.int(p.Threads), C.int(h264Level(p.W, p.H, fps)), cp)
	if e == nil {
		return nil, errors.New("x264 open failed")
	}
	return &videoEncoder{e: e, w: p.W, h: p.H}, nil
}

func presetOK(s string) bool {
	switch s {
	case "ultrafast", "superfast", "veryfast", "faster", "fast":
		return true
	}
	return false
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
