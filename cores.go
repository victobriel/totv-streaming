// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 victobriel

package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// coreInfo describes one libretro core the component may run. The key of coreTable is the file name without
// "_libretro.so" (the Totv calls it "server_<key>").
type coreInfo struct {
	platform string
	gl       bool // HW_RENDER core: needs the GPU context the host creates before the sandbox
	devOnly  bool // only with TOTV_STREAM_DEV_TESTCORE=1

	// need_fullpath cores (PSP): the ROM is a file of hundreds of MB to GB that the core opens itself. It is downloaded to a directory
	// that holds only it (Landlock read-only), never read into memory by the host, and the core gets one writable directory for its
	// "memory stick". Its save is a directory, packed as a zip by the parent (savezip.go).
	fullPath  bool
	dirSave   bool  // the save is the directory <rw>/PSP/SAVEDATA, a zip on the Totv side
	needShm   bool  // builds its emulated RAM as a shm file: gets a PRIVATE tmpfs on /dev/shm (host option shm=1)
	sysAssets bool  // needs <system dir>/PPSSPP (ppge_atlas.zim, flash0 fonts...), mounted read-only
	maxROM    int64 // download limit for the ROM; 0 = the 64 MiB of the other platforms
	outW      int   // stream size of this core (0 = TOTV_STREAM_WIDTH/HEIGHT): PSP is 16:9, 848x480
	outH      int
	startWait time.Duration // how long POST /v1/sessions may take (an ISO download); 0 = 12 s
}

var coreTable = map[string]coreInfo{
	"snes9x":           {platform: "snes"},
	"testcore":         {platform: "snes", devOnly: true},
	"mupen64plus_next": {platform: "n64", gl: true},
	"glcore":           {platform: "n64", gl: true, devOnly: true}, // triangle, no ROM (tests the GPU path)
	"glhogt":           {platform: "n64", gl: true, devOnly: true}, // glcore that makes a thread bomb after 1 s (tests the per-session cgroup)
	"glhogm":           {platform: "n64", gl: true, devOnly: true}, // glcore that eats memory after 1 s
	// PPSSPP: OpenGL (core profile 3.1 that the host's 4.6 context satisfies), 480x272 native, 16:9
	"ppsspp": {platform: "psp", gl: true, fullPath: true, dirSave: true, needShm: true, sysAssets: true, maxROM: maxPSPROM, outW: 848, outH: 480, startWait: 60 * time.Second},
	// test core with the PSP's contract (need_fullpath, directory save, no SAVE_RAM) and no GPU
	"pspcore": {platform: "psp", fullPath: true, dirSave: true, needShm: true, devOnly: true, maxROM: maxPSPROM, startWait: 20 * time.Second},
}

// maxPSPROM: a UMD holds 1.8 GB at most (dual layer), a CSO/PBP is smaller.
const maxPSPROM = 2 << 30

// size is the stream size for a core: its own (PSP 16:9) or the global one.
func (s *server) size(ci coreInfo) (int, int) {
	if ci.outW > 0 && ci.outH > 0 {
		return ci.outW, ci.outH
	}
	return s.cfg.W, s.cfg.H
}

func (s *server) coreAllowed(name string) (coreInfo, bool) {
	ci, ok := coreTable[name]
	if !ok || (ci.devOnly && !s.cfg.DevTestCore) {
		return coreInfo{}, false
	}
	return ci, true
}

// hostOptions builds the 8th argument of retro-emu ("k=v;k=v", see emu/host.c): the GL context to create before the
// sandbox, the GPU device nodes the sandbox still lets the core open, the readback mode and the core's own options.
func (s *server) hostOptions(name string, ci coreInfo, saveDir, assetsDir string) string {
	var o []string
	o = append(o, "fsize="+itoa64(s.cfg.fileMax()))
	if ci.dirSave && okOptPath(saveDir) {
		o = append(o, "save="+saveDir)
	}
	if ci.needShm {
		o = append(o, "shm=1")
	}
	if ci.sysAssets && okOptPath(assetsDir) {
		o = append(o, "ro="+assetsDir)
	}
	if !ci.gl {
		return strings.Join(o, ";")
	}
	o = append(o, "gl=core46")
	for _, d := range strings.Split(s.cfg.GPUDevices, ",") {
		if d = strings.TrimSpace(d); okGPUDevice(d) {
			o = append(o, "dev="+d)
		}
	}
	if s.cfg.ReadbackPBO {
		o = append(o, "rb=pbo")
	}
	if name == "ppsspp" {
		o = append(o, pspCoreOptions(s.cfg.PSPCoreOpts)...)
	}
	if name == "mupen64plus_next" {
		res := "320x240"
		if s.cfg.N64Internal2x {
			res = "640x480"
		}
		o = append(o,
			"v.mupen64plus-43screensize="+res,
			"v.mupen64plus-alt-map=True", // independent C buttons (see input.go)
			"v.mupen64plus-rdp-plugin=gliden64", "v.mupen64plus-rsp-plugin=hle", "v.mupen64plus-cpucore=dynamic_recompiler")
		o = append(o, n64VideoOptions(s.cfg.N64Overscan, s.cfg.N64Aspect)...)
		for _, kv := range strings.Split(s.cfg.N64CoreOpts, ";") {
			if strings.HasPrefix(kv, "mupen64plus-") && strings.Count(kv, "=") == 1 {
				o = append(o, "v."+kv)
			}
		}
	}
	return strings.Join(o, ";")
}

// defaultN64Overscan crops the black frame the N64 video interface leaves around the picture (GLideN64 overscan, in pixels of the N64's
// 320x240 frame: left,right,top,bottom). Measured on a commercial N64 game: the active area of the 640x480 stream was x 38..613, y 23..451, i.e. 19/14/11/14
// native pixels of border; the remaining 287x215 is 4:3 within 0.1%, so the aspect stays right and nothing is stretched. Games differ a little
// (the border comes from each game's VI setup); override with TOTV_STREAM_N64_OVERSCAN="l,r,t,b" or "off".
const defaultN64Overscan = "19,14,11,14"

// n64VideoOptions maps the overscan crop and the aspect setting to host "v.<core option>" entries. Invalid input falls back to the defaults
// (never to an unvalidated string). aspect: "" / "4:3" (default, picture is 4:3 and is NOT stretched) or "16:9" / "16:9 adjusted", the core's own
// widescreen rendering (needs a 16:9 stream: TOTV_STREAM_WIDTH/HEIGHT, and not every game supports it).
func n64VideoOptions(overscan, aspect string) []string {
	var o []string
	switch strings.TrimSpace(overscan) {
	case "off", "none":
		o = append(o, "v.mupen64plus-EnableOverscan=Disabled")
	default:
		v := [4]int{}
		parts := strings.Split(strings.TrimSpace(overscan), ",")
		ok := len(parts) == 4
		for i := 0; ok && i < 4; i++ {
			n, err := strconv.Atoi(strings.TrimSpace(parts[i]))
			if err != nil || n < 0 || n > 60 {
				ok = false
			}
			v[i] = n
		}
		if !ok {
			fmt.Sscanf(defaultN64Overscan, "%d,%d,%d,%d", &v[0], &v[1], &v[2], &v[3])
		}
		o = append(o, "v.mupen64plus-EnableOverscan=Enabled", fmt.Sprintf("v.mupen64plus-OverscanLeft=%d", v[0]), fmt.Sprintf("v.mupen64plus-OverscanRight=%d", v[1]),
			fmt.Sprintf("v.mupen64plus-OverscanTop=%d", v[2]), fmt.Sprintf("v.mupen64plus-OverscanBottom=%d", v[3]))
	}
	switch strings.TrimSpace(aspect) {
	case "16:9":
		o = append(o, "v.mupen64plus-aspect=16:9")
	case "16:9 adjusted":
		o = append(o, "v.mupen64plus-aspect=16:9 adjusted")
	}
	return o
}

// hostMs are the per-stage times the host sends every 120 frames (message 'T', 12 float32).
type hostMs struct {
	Core, Blit, Readback, Convert, Total                float32
	CoreMax, BlitMax, ReadbackMax, ConvertMax, TotalMax float32
	GPU                                                 float32
}

func (h hostMs) String() string {
	return fmt.Sprintf("core %.2f blit %.2f readback %.2f convert %.2f total %.2f ms (max %.1f) gpu %.2f", h.Core, h.Blit, h.Readback, h.Convert, h.Total, h.TotalMax, h.GPU)
}

// okOptPath: a directory handed to the host in the "k=v;k=v" option string must be absolute and free of the separators.
func okOptPath(p string) bool {
	return strings.HasPrefix(p, "/") && !strings.ContainsAny(p, ";=\x00\n") && !strings.Contains(p, "..")
}

// pspCoreOptions are the PPSSPP libretro core options the component sets (names: libretro/libretro_core_options.h of PPSSPP).
// Native 1x resolution (480x272) keeps the GPU light; the rest are what makes the core start sanely headless: its own frame
// skipping and audio latency are left alone. The backend stays "auto": the core then asks the host for its preferred type (the host
// answers OpenGL core, which it created) and builds LibretroGLCoreContext (core profile 3.1); forcing "opengl" would make it ask for a
// COMPATIBILITY context, which the host refuses on purpose. The in-core shader cache lives in <save dir>/PSP/SYSTEM/CACHE, i.e. in the
// session's tmpfs directory, and the save package never contains it.
// extra: TOTV_STREAM_PSP_CORE_OPTS="ppsspp_x=y;..." (only keys starting with ppsspp_).
func pspCoreOptions(extra string) []string {
	o := []string{
		"v.ppsspp_internal_resolution=480x272",
		"v.ppsspp_cpu_core=JIT",
		"v.ppsspp_frameskip=disabled",
		"v.ppsspp_cache_iso=disabled",        // enabled would copy the whole ISO into RAM
		"v.ppsspp_inflight_frames=No buffer", // measured: -17 ms of input latency (README), the GPU is not the limit here
		"v.ppsspp_auto_frameskip=disabled",
	}
	for _, kv := range strings.Split(extra, ";") {
		if strings.HasPrefix(kv, "ppsspp_") && strings.Count(kv, "=") == 1 && printable(kv) { // values may hold spaces ("No buffer", "IR JIT")
			key := "v." + strings.SplitN(kv, "=", 2)[0] + "="
			kept := o[:0] // the host answers with the FIRST matching key: an override must replace the default, not follow it
			for _, d := range o {
				if !strings.HasPrefix(d, key) {
					kept = append(kept, d)
				}
			}
			o = append(kept, "v."+kv)
		}
	}
	return o
}

func printable(s string) bool {
	for _, r := range s {
		if r < 0x20 || r > 0x7e {
			return false
		}
	}
	return true
}
