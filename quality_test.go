// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 victobriel

package main

import (
	"bytes"
	"strings"
	"testing"
)

// A zero config (what the older tests build) stays on the previous picture: 640x480, PSP 848x480, native scale, a 4 Mbps ceiling.
func TestProfileSafeIsTheOldBehaviour(t *testing.T) {
	s := &server{cfg: config{W: 640, H: 480}}
	for name, want := range map[string][2]int{"mupen64plus_next": {640, 480}, "ppsspp": {848, 480}, "pcsx2": {640, 480}, "snes9x": {640, 480}} {
		p := s.profile(coreTable[name], "")
		if p.Quality != "safe" || p.W != want[0] || p.H != want[1] || p.Kbps != 4000 {
			t.Errorf("%s: %+v", name, p)
		}
	}
	if p := s.profile(coreTable["ppsspp"], ""); p.PSPRes != "480x272" {
		t.Errorf("psp res %s", p.PSPRes)
	}
	if p := s.profile(coreTable["pcsx2"], ""); p.PS2Mult != "1x (Native)" {
		t.Errorf("ps2 mult %s", p.PS2Mult)
	}
	if p := s.profile(coreTable["mupen64plus_next"], ""); p.N64Opt != "320x240" {
		t.Errorf("n64 opt %s", p.N64Opt)
	}
}

func TestProfileHigh(t *testing.T) {
	s := &server{cfg: config{W: 640, H: 480, Quality: "high"}}
	check := func(name string, w, h, kbps int, opt string) {
		t.Helper()
		p := s.profile(coreTable[name], "")
		got := map[string]string{"mupen64plus_next": p.N64Opt, "ppsspp": p.PSPRes, "pcsx2": p.PS2Mult, "snes9x": ""}[name]
		if p.Quality != "high" || p.W != w || p.H != h || p.Kbps != kbps || got != opt {
			t.Errorf("%s: %+v (want %dx%d %d %q)", name, p, w, h, kbps, opt)
		}
	}
	check("mupen64plus_next", 1280, 720, 12000, "640x480")
	check("ppsspp", 960, 544, 6800, "960x544")
	check("pcsx2", 1280, 720, 12000, "2x")
	check("snes9x", 640, 480, 4000, "") // software core: not touched
	// a session can ask for the safe profile, whatever the server default is
	if p := s.profile(coreTable["mupen64plus_next"], "safe"); p.W != 640 || p.N64Opt != "320x240" || p.Kbps != 4000 {
		t.Errorf("safe request: %+v", p)
	}
	// per-platform size and the global size override; the ceiling follows the pixels unless TOTV_STREAM_KBPS says
	s.cfg.PS2Size = "1280x960"
	if p := s.profile(coreTable["pcsx2"], ""); p.W != 1280 || p.H != 960 || p.Kbps != 16000 {
		t.Errorf("ps2 size: %+v", p)
	}
	s.cfg.PS2Size = "1281x960" // odd width: refused, default kept
	if p := s.profile(coreTable["pcsx2"], ""); p.W != 1280 || p.H != 720 {
		t.Errorf("odd size accepted: %+v", p)
	}
	s.cfg.SizeSet, s.cfg.Kbps, s.cfg.N64Res = true, 5000, "native"
	if p := s.profile(coreTable["mupen64plus_next"], ""); p.W != 640 || p.Kbps != 5000 || p.N64Opt != "320x240" {
		t.Errorf("global size/ceiling: %+v", p)
	}
}

func TestHostOptionsFollowProfile(t *testing.T) {
	s := &server{cfg: config{Quality: "high"}}
	if o := s.hostOptions("ppsspp", coreTable["ppsspp"], "/tmp/rw-1", "/opt/cores/system"); !strings.Contains(o, "v.ppsspp_internal_resolution=960x544") {
		t.Errorf("psp: %s", o)
	}
	if o := s.hostOptions("mupen64plus_next", coreTable["mupen64plus_next"], "", ""); !strings.Contains(o, "v.mupen64plus-43screensize=640x480") {
		t.Errorf("n64: %s", o)
	}
	if o := strings.Join(ps2CoreOptionsMult("", "", "2x"), ";"); !strings.Contains(o, "v.pcsx2_upscale_multiplier=2x") || strings.Contains(o, "Native") {
		t.Errorf("ps2: %s", o)
	}
	// an explicit core option still wins over the profile
	if o := strings.Join(ps2CoreOptionsMult("pcsx2_upscale_multiplier=4x", "", "2x"), ";"); !strings.Contains(o, "pcsx2_upscale_multiplier=4x") || strings.Contains(o, "=2x") {
		t.Errorf("ps2 override: %s", o)
	}
}

func TestH264LevelAndFmtp(t *testing.T) {
	for _, c := range []struct {
		w, h int
		fps  float64
		want int
	}{{640, 480, 60, 31}, {848, 480, 59.94, 31}, {960, 544, 59.94, 32}, {960, 720, 60, 32}, {1280, 720, 60, 32}, {1280, 960, 60, 42}, {1920, 1080, 60, 42}} {
		if got := h264Level(c.w, c.h, c.fps); got != c.want {
			t.Errorf("%dx%d@%v: level %d, want %d", c.w, c.h, c.fps, got, c.want)
		}
	}
	if got := h264FmtpFor(32); !strings.Contains(got, "profile-level-id=42e020") || !strings.Contains(got, "level-asymmetry-allowed=1") {
		t.Errorf("fmtp %s", got)
	}
	if h264FmtpFor(31) != h264Fmtp {
		t.Errorf("level 3.1 must equal the old constant: %s", h264FmtpFor(31))
	}
}

func TestAnnounceLevel(t *testing.T) {
	in := "v=0\r\nm=video 9 UDP/TLS/RTP/SAVPF 102\r\na=rtpmap:102 H264/90000\r\na=fmtp:102 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f\r\na=fmtp:111 minptime=10;useinbandfec=1\r\n"
	got := announceLevel(in, 32)
	if !strings.Contains(got, "profile-level-id=42e020\r\n") || strings.Contains(got, "42e01f") || !strings.Contains(got, "a=fmtp:111 minptime=10;useinbandfec=1\r\n") {
		t.Errorf("answer: %q", got)
	}
	if o := "a=fmtp:97 packetization-mode=1;profile-level-id=640c1f\r\n"; announceLevel(o, 32) != o {
		t.Error("a non-Baseline line was rewritten")
	}
}

func TestFpsRational(t *testing.T) {
	for in, want := range map[float64][2]int{60: {60, 1}, 50: {50, 1}, 59.94: {60000, 1001}, 59.940059940059946: {60000, 1001}, 29.97: {30000, 1001}, 0: {60, 1}, 500: {60, 1}, 30: {30, 1}} {
		if n, d := fpsRational(in); n != want[0] || d != want[1] {
			t.Errorf("%v: %d/%d, want %v", in, n, d, want)
		}
	}
}

// the encoder opens at every supported size, announces Constrained Baseline at the level of the picture, and produces a decodable-looking IDR first
func TestEncoderCRFLevels(t *testing.T) {
	for _, c := range []struct {
		w, h int
		fps  float64
		lvl  byte
	}{{640, 480, 60, 31}, {960, 720, 50, 32}, {960, 544, 59.94, 32}, {1280, 960, 60, 42}} {
		e, err := newVideoEncoder(encoderParams{W: c.w, H: c.h, FPS: c.fps, MaxKbps: 8000, BufFrames: 6})
		if err != nil {
			t.Fatalf("%dx%d: %v", c.w, c.h, err)
		}
		frame := make([]byte, c.w*c.h*3/2)
		for i := range frame {
			frame[i] = byte(i * 7)
		}
		au, err := e.Encode(frame, true)
		e.Close()
		if err != nil || len(au) == 0 {
			t.Fatalf("%dx%d: encode %v len %d", c.w, c.h, err, len(au))
		}
		i := bytes.Index(au, []byte{0, 0, 0, 1, 0x67})
		if i < 0 || au[i+5] != 66 || au[i+6]&0x40 == 0 || au[i+7] != c.lvl { // profile_idc 66, constraint_set1 (constrained baseline), level_idc
			t.Errorf("%dx%d: SPS % x, want baseline level %d", c.w, c.h, au[:12], c.lvl)
		}
	}
	if _, err := newVideoEncoder(encoderParams{W: 960, H: 720, FPS: 60, MaxKbps: 8000, Preset: "placebo; rm"}); err != nil {
		t.Errorf("an unknown preset must fall back, not fail: %v", err)
	}
}

func TestCheckBudget(t *testing.T) {
	s := &session{id: "b", srv: &server{cfg: config{HostMsP95: 12}}, prof: outProfile{Quality: "high", W: 960, H: 720}}
	slow, fast := &hostMs{TotalP95: 14}, &hostMs{TotalP95: 6}
	for i := 0; i < hostBudgetWindows-1; i++ {
		s.checkBudget(slow)
	}
	if s.overBudget.Load() {
		t.Fatal("flagged before the window count")
	}
	s.checkBudget(slow)
	if !s.overBudget.Load() || s.suggestQuality() != "safe" {
		t.Fatal("not flagged after 3 slow windows")
	}
	s.checkBudget(fast)
	if s.overBudget.Load() || s.suggestQuality() != "" {
		t.Fatal("not cleared by a fast window")
	}
	s.srv.cfg.HostMsP95 = -1 // off
	for i := 0; i < 5; i++ {
		s.checkBudget(slow)
	}
	if s.overBudget.Load() {
		t.Fatal("flagged with the check off")
	}
	s.prof.Quality = "safe"
	s.srv.cfg.HostMsP95 = 0 // default 12 ms
	for i := 0; i < 3; i++ {
		s.checkBudget(slow)
	}
	if !s.overBudget.Load() || s.suggestQuality() != "" {
		t.Fatal("a safe session reports the overrun but has nothing lower to suggest")
	}
}
