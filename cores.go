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
	maxROM    int64 // download limit for the ROM; 0 = the 64 MiB of the other platforms (PS2: TOTV_STREAM_PS2_MAX_ISO_MB, see cfg.romMax)
	bigROM    bool  // images of GB (PS2): the session directory goes to TOTV_STREAM_BIG_TMP (a disk directory), never to a RAM tmpfs (ps2.go)
	needBIOS  bool  // needs a console BIOS the operator provides (TOTV_STREAM_PS2_BIOS_DIR); the component never ships one (ps2.go)
	saveSys   bool  // directory save below the SYSTEM directory (PS2: <rw>/pcsx2/memcards); the system directory is then the writable one
	memMB     int   // per-session memory.max in MiB for this core (0 = TOTV_STREAM_SESSION_MEM_MB)
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
	// PCSX2 (LRPS2 / "parallel-ps2", GPL-3.0): OpenGL 4.6 renderer on the host's context, JIT, 4 GiB of fastmem on a private /dev/shm. Needs a
	// console BIOS (never shipped) and an ISO of up to 9 GiB by path. See README, "PS2".
	"pcsx2": {platform: "ps2", gl: true, fullPath: true, dirSave: true, needShm: true, needBIOS: true, saveSys: true, bigROM: true, maxROM: maxPS2ROM, memMB: ps2SessionMemMB, outW: 640, outH: 480, startWait: 10 * time.Minute},
	// test core with the PS2's contract (BIOS in <system>/pcsx2/bios, memory cards in <system>/pcsx2/memcards, ISO by path, RWX JIT-like mapping), no GPU
	"ps2core": {platform: "ps2", fullPath: true, dirSave: true, needShm: true, needBIOS: true, saveSys: true, bigROM: true, devOnly: true, maxROM: maxPS2ROM, memMB: ps2SessionMemMB, startWait: 60 * time.Second},
	// test core with the PSP's contract (need_fullpath, directory save, no SAVE_RAM) and no GPU
	"pspcore": {platform: "psp", fullPath: true, dirSave: true, needShm: true, devOnly: true, maxROM: maxPSPROM, startWait: 20 * time.Second},
}

// maxPS2ROM: a dual-layer DVD image is up to 8.5 GB (~7.9 GiB); 9 GiB leaves room. TOTV_STREAM_PS2_MAX_ISO_MB lowers or raises it.
const maxPS2ROM = 9 << 30

// ps2SessionMemMB: PCSX2 measured 780 MB (RSS, of which 260 MB shm) at the BIOS idle with a synthetic BIOS; a real game adds the GS targets, the texture
// cache and the driver's own memory, ~1-2 GiB is what PCSX2 users report. 4 GiB (memory.max, also counts the private /dev/shm) leaves 2x room.
const ps2SessionMemMB = 4096

// maxPSPROM: a UMD holds 1.8 GB at most (dual layer), a CSO/PBP is smaller.
const maxPSPROM = 2 << 30

// size is the stream size for a core under the server's default quality (see profile).
func (s *server) size(ci coreInfo) (int, int) {
	p := s.profile(ci, "")
	return p.W, p.H
}

func qualityName(q string) string {
	if q == "high" {
		return "high"
	}
	return "safe"
}

// outProfile is everything a session's picture quality depends on: the fixed stream size, the cores' internal render scale, and the bitrate ceiling.
type outProfile struct {
	Quality string
	Aspect  string // initial picture placement ("" = auto), see aspectNames
	W, H    int
	Kbps    int    // VBV ceiling of the x264 CRF encoder
	N64x2   bool   // N64 at 640x480 internal (3x: N64Mult)
	N64Opt  string // mupen64plus-43screensize
	PSPRes  string // ppsspp_internal_resolution
	PS2Mult string // pcsx2_upscale_multiplier
}

// defaultHighSize: the stream is a fixed 16:9 frame so that the picture can be re-placed live (aspect modes) without restarting the encoder: the 4:3
// platforms (N64, PS2) at 1280x720, where "auto" shows them as a 960x720 pillarbox picture (1.5x of the old 640x480 and a whole 3x of the N64's 320x240),
// the PSP at 960x544 (its 16:9 at 2x native).
func defaultHighSize(platform string) (int, int) {
	if platform == "psp" {
		return 960, 544
	}
	return 1280, 720
}

func parseSize(s string) (int, int, bool) {
	var w, h int
	if n, err := fmt.Sscanf(strings.TrimSpace(s), "%dx%d", &w, &h); err != nil || n != 2 || w < 160 || h < 120 || w > 1920 || h > 1088 || w%2 != 0 || h%2 != 0 {
		return 0, 0, false
	}
	return w, h, true
}

// autoKbps is the VBV ceiling for a picture size: 9 Mbps at 960x720 (12 Mbps at 1280x720, under the 20 Mbps of level 3.2), proportional to the pixels, between 3 and 16 Mbps.
func autoKbps(w, h int) int {
	k := w * h * 9000 / (960 * 720)
	if k < 3000 {
		k = 3000
	}
	if k > 16000 {
		k = 16000
	}
	return k
}

// profile resolves the quality of a session ("high", "safe" or "" = the server default). "safe" is the previous behaviour: 640x480 (PSP 848x480), N64 at
// its native 320x240 unless TOTV_STREAM_N64_RES=2x, 1x PSP/PS2, 4 Mbps ceiling. A zero config{} (tests) is "safe".
func (s *server) profile(ci coreInfo, quality string) outProfile {
	c := s.cfg
	if quality == "" {
		quality = c.Quality
	}
	p := outProfile{Quality: qualityName(quality), N64Opt: "320x240", PSPRes: "480x272", PS2Mult: "1x (Native)"}
	if c.N64Internal2x {
		p.N64Opt = "640x480"
	}
	if p.Quality != "high" {
		p.W, p.H = c.W, c.H
		if ci.outW > 0 && ci.outH > 0 {
			p.W, p.H = ci.outW, ci.outH
		}
		p.Kbps = c.Kbps
		if p.Kbps <= 0 {
			p.Kbps = 4000
		}
		return p
	}
	// high
	switch ci.platform {
	case "n64":
		p.N64Opt = map[string]string{"native": "320x240", "1x": "320x240", "3x": "960x720"}[c.N64Res]
		if p.N64Opt == "" {
			p.N64Opt = "640x480"
		}
	case "psp":
		p.PSPRes = map[string]string{"1x": "480x272", "3x": "1440x816"}[c.PSPRes]
		if p.PSPRes == "" {
			p.PSPRes = "960x544"
		}
	case "ps2":
		p.PS2Mult = map[string]string{"1x": "1x (Native)", "4x": "4x"}[c.PS2Res]
		if p.PS2Mult == "" {
			p.PS2Mult = "2x"
		}
	}
	if ci.platform == "n64" || ci.platform == "psp" || ci.platform == "ps2" {
		p.W, p.H = defaultHighSize(ci.platform)
	} else {
		p.W, p.H = c.W, c.H
		if ci.outW > 0 && ci.outH > 0 {
			p.W, p.H = ci.outW, ci.outH
		}
	}
	if c.SizeSet && ci.platform != "snes" {
		p.W, p.H = c.W, c.H
	}
	own := map[string]string{"n64": c.N64Size, "psp": c.PSPSize, "ps2": c.PS2Size}[ci.platform]
	if w, h, ok := parseSize(own); ok {
		p.W, p.H = w, h
	}
	p.Kbps = c.Kbps
	if p.Kbps <= 0 {
		p.Kbps = autoKbps(p.W, p.H)
	}
	return p
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
	return s.hostOptionsBIOS(name, ci, saveDir, assetsDir, "")
}

// hostOptionsBIOS is hostOptions plus the BIOS file name (inside <system>/pcsx2/bios) the PS2 core must use.
func (s *server) hostOptionsBIOS(name string, ci coreInfo, saveDir, assetsDir, biosName string) string {
	return s.hostOptionsProfile(name, ci, saveDir, assetsDir, biosName, s.profile(ci, ""))
}

// hostOptionsProfile is hostOptionsBIOS for one session's quality profile.
func (s *server) hostOptionsProfile(name string, ci coreInfo, saveDir, assetsDir, biosName string, pr outProfile) string {
	var o []string
	o = append(o, "fsize="+itoa64(s.cfg.fileMax()))
	if aspectCode(pr.Aspect) > 0 {
		o = append(o, "aspect="+pr.Aspect)
	}
	if ci.dirSave && okOptPath(saveDir) {
		o = append(o, "save="+saveDir)
	}
	if ci.needShm {
		o = append(o, "shm=1")
		if ci.platform == "ps2" {
			o = append(o, "shmmb=1024") // PCSX2's main map (4 GiB, sparse) lives in it: 260 MB touched at the BIOS idle; 512 MiB is the PSP's default
		}
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
		o = append(o, pspCoreOptionsRes(s.cfg.PSPCoreOpts, pr.PSPRes)...)
	}
	if name == "pcsx2" {
		o = append(o, ps2CoreOptionsMult(s.cfg.PS2CoreOpts, biosName, pr.PS2Mult)...)
	}
	if name == "mupen64plus_next" {
		o = append(o,
			"v.mupen64plus-43screensize="+pr.N64Opt,
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
	GPU, TotalP95                                       float32 // GPU: glTimer span; TotalP95: p95 of the per-frame total in the 120-frame window
}

func (h hostMs) String() string {
	return fmt.Sprintf("core %.2f blit %.2f readback %.2f convert %.2f total %.2f ms (p95 %.1f, max %.1f) gpu %.2f", h.Core, h.Blit, h.Readback, h.Convert, h.Total, h.TotalP95, h.TotalMax, h.GPU)
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
func pspCoreOptions(extra string) []string { return pspCoreOptionsRes(extra, "480x272") }

// pspCoreOptionsRes is pspCoreOptions at an internal resolution (480x272 native, 960x544 = 2x).
func pspCoreOptionsRes(extra, res string) []string {
	o := []string{
		"v.ppsspp_internal_resolution=" + res,
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

// ps2CoreOptions are the PCSX2 libretro core options (names: libretro/libretro_core_options.h of the core). OpenGL is the only renderer the host
// can serve (it creates the GL 4.6 core context; Vulkan is not built, see README "PS2"); 1x native resolution keeps the GPU light; the pad starts in
// analog mode (a DualShock 2 game that expects the sticks would otherwise wait for the ANALOG button, which the packet has no bit for). biosName was
// validated by ps2BiosScan (a plain file name, no separator). extra: TOTV_STREAM_PS2_CORE_OPTS="pcsx2_x=y;..." (only keys starting with pcsx2_,
// never pcsx2_bios: the BIOS is chosen by the component).
func ps2CoreOptions(extra, biosName string) []string {
	return ps2CoreOptionsMult(extra, biosName, "1x (Native)")
}

// ps2CoreOptionsMult is ps2CoreOptions at an upscale multiplier ("1x (Native)", "2x", "4x").
func ps2CoreOptionsMult(extra, biosName, mult string) []string {
	o := []string{
		"v.pcsx2_renderer=OpenGL",
		"v.pcsx2_upscale_multiplier=" + mult,
		"v.pcsx2_analog_mode1=enabled",
		"v.pcsx2_analog_mode2=enabled",
		"v.pcsx2_shared_memory_cards=enabled", // one pair of cards per session, in <system>/pcsx2/memcards: that is the save
	}
	if biosName != "" && ps2BiosNameOK(biosName) {
		o = append(o, "v.pcsx2_bios="+biosName)
	}
	for _, kv := range strings.Split(extra, ";") {
		if strings.HasPrefix(kv, "pcsx2_") && !strings.HasPrefix(kv, "pcsx2_bios=") && !strings.HasPrefix(kv, "pcsx2_shared_memory_cards=") && strings.Count(kv, "=") == 1 && printable(kv) {
			key := "v." + strings.SplitN(kv, "=", 2)[0] + "="
			kept := o[:0]
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
