// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 victobriel

package main

import (
	"bytes"
	"crypto/md5"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- real PS2 (PS2=1): the PCSX2 libretro core (cores/pcsx2_libretro.so, scripts/fetch-cores.sh) on the GPU ----
//
// Two modes, one test body:
//   - SYNTHETIC (no PS2_ROM / PS2_BIOS_DIR): a BIOS image this repository writes (a valid ROMDIR around a "b ." loop: no console code in it) and a junk
//     image. The EE runs the loop; nothing is ever drawn. It proves what does not need a game: the real core loads, the GL context, the JIT (RWX),
//     the 4 GiB fastmem on the private /dev/shm, the threads and the memory under the full sandbox, the cards the core formats and the zip round trip.
//   - REAL (PS2_ROM=<image outside the repo>, PS2_BIOS_DIR=<directory with YOUR OWN console BIOS>): the same, plus decoded video and input.
//
// Env: PS2_SECS (synthetic 12, real 60), PS2_OUT (PNG dir), PS2_CORE_OPTS ("pcsx2_x=y;..."), PS2_DRIVE_BUTTONS (comma list of cross,circle,square,
// triangle,l,r,start,select,up,down,left,right; default start,cross), PS2_DRIVE_START (s, default 20), PS2_DRIVE_EVERY (s, default 3).

func ps2RealEnv(t *testing.T) (e *testEnv, real bool, biosDir string) {
	t.Helper()
	if os.Getenv("PS2") != "1" {
		t.Skip("set PS2=1 (make ps2); add PS2_ROM=<image> PS2_BIOS_DIR=<dir> for the real game test")
	}
	cwd, _ := os.Getwd()
	coresDir := filepath.Join(cwd, "cores")
	if _, err := os.Stat(filepath.Join(coresDir, "pcsx2_libretro.so")); err != nil {
		t.Skip("cores/pcsx2_libretro.so not built (scripts/fetch-cores.sh)")
	}
	rom, bios := os.Getenv("PS2_ROM"), os.Getenv("PS2_BIOS_DIR")
	if _, err := os.Stat(rom); rom != "" && err != nil {
		t.Skipf("PS2_ROM not readable: %v", err)
	}
	real = rom != "" && bios != ""
	if !real {
		bios = writeBIOSDir(t, map[string][]byte{"scph50004.bin": syntheticPS2BIOS()})
		rom = fakePS2ISO(t, 3<<20, "")
		// a junk image: the core refuses its volume descriptor and goes on to the BIOS, which loops
		os.WriteFile(rom, bytes.Repeat([]byte{0x5a}, 3<<20), 0o600)
	}
	rw := t.TempDir()
	e = newEnv(t, func(c *config) {
		c.CoresDir, c.DevTestCore, c.RWTmp, c.DirSaveEvery, c.RWAllowDisk = coresDir, false, rw, 5*time.Second, true
		c.PS2BiosDir, c.BigTmp, c.PS2CoreOpts = bios, diskTmp(t), os.Getenv("PS2_CORE_OPTS")
		c.NoVideoTimeout = 10 * time.Minute // the synthetic BIOS never draws
		c.NoSignalTimeout = 10 * time.Minute
	})
	skipWithoutGPU(t, e)
	e.totv.romPath = rom
	return e, real, bios
}

func ps2RealBody(e *testEnv, id string) map[string]any {
	b := e.createBody(id)
	b["platform"], b["core"] = "ps2", "server_pcsx2"
	return b
}

func ps2ThreadReport(t *testing.T, pid int) (threads, seccomp2 int) {
	t.Helper()
	tasks, _ := filepath.Glob(fmt.Sprintf("/proc/%d/task/*/status", pid))
	for _, f := range tasks {
		b, _ := os.ReadFile(f)
		threads++
		if strings.Contains(string(b), "Seccomp:\t2") && strings.Contains(string(b), "NoNewPrivs:\t1") {
			seccomp2++
		}
	}
	return
}

func ps2ProcField(pid int, key string) string {
	b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, key+":") {
			return strings.TrimSpace(strings.TrimPrefix(l, key+":"))
		}
	}
	return ""
}

func TestRealPS2(t *testing.T) {
	e, real, _ := ps2RealEnv(t)
	secs := pspEnvInt("PS2_SECS", 12)
	if real {
		secs = pspEnvInt("PS2_SECS", 60)
	}
	out := os.Getenv("PS2_OUT")
	if out == "" {
		out = t.TempDir()
	}
	os.MkdirAll(out, 0o755)
	if _, m, _ := e.call("GET", "/v1/health", nil, testSecret); m["cores"].(map[string]any)["ps2"] == nil {
		t.Fatalf("health does not offer the ps2 core: %v", m)
	}
	t0 := time.Now()
	code, m, raw := e.call("POST", "/v1/sessions", ps2RealBody(e, "ps2r"), testSecret)
	if code != 201 {
		t.Fatalf("create: %d %v %s", code, m, raw)
	}
	t.Logf("real=%v: session up in %.1f s", real, time.Since(t0).Seconds())
	e.srv.mu.Lock()
	ss := e.srv.sessions["ps2r"]
	e.srv.mu.Unlock()
	if ss.flags&(1|2|4|8|16|32) != 1|2|4|8|16|32 {
		t.Fatalf("sandbox flags %d: want seccomp, landlock, landlock net, netns, GPU context, private /dev/shm", ss.flags)
	}
	pid := ss.corePID()
	p := newTestPeer(t)
	defer p.pc.Close()
	w, h := e.srv.size(coreTable["pcsx2"])
	d := startDecoder(t, p, w, h)
	p.negotiate(t, e, "ps2r")
	if real {
		waitFor(t, "decoded video", 90*time.Second, func() bool { _, n := d.grab(); return n > 5 })
	}
	hashes := map[[16]byte]int{}
	var lastH [16]byte
	schedule := ps2Schedule()
	start, every := time.Duration(pspEnvInt("PS2_DRIVE_START", 20))*time.Second, time.Duration(pspEnvInt("PS2_DRIVE_EVERY", 3))*time.Second
	nextPress, pi, nextShot := start, 0, 10*time.Second
	tStart := time.Now()
	for el := time.Duration(0); el < time.Duration(secs)*time.Second; el = time.Since(tStart) {
		if real && el >= nextPress && len(schedule) > 0 {
			p.sendInput(schedule[pi%len(schedule)])
			pi++
			time.Sleep(150 * time.Millisecond)
			p.sendInput(0)
			nextPress = el + every
		}
		b, _ := d.grab()
		hs := md5.Sum(b)
		if hs != lastH {
			hashes[hs]++
			lastH = hs
		}
		if real && el >= nextShot {
			d.savePNG(t, filepath.Join(out, fmt.Sprintf("ps2-t%03d.png", int(el.Seconds()))), b)
			nextShot += 10 * time.Second
		}
		time.Sleep(250 * time.Millisecond)
	}
	// what the sandbox looks like from the kernel's side, with the core running
	th, sc := ps2ThreadReport(t, pid)
	t.Logf("child: %d threads, %d with seccomp filter mode 2 + no_new_privs; VmHWM %s VmRSS %s RssAnon %s RssShmem %s CapEff %s", th, sc, ps2ProcField(pid, "VmHWM"), ps2ProcField(pid, "VmRSS"),
		ps2ProcField(pid, "RssAnon"), ps2ProcField(pid, "RssShmem"), ps2ProcField(pid, "CapEff"))
	if th == 0 || th != sc {
		t.Errorf("%d of %d threads are under the filter (every thread of the core must be)", sc, th)
	}
	if ps2ProcField(pid, "CapEff") != "0000000000000000" {
		t.Errorf("CapEff %s", ps2ProcField(pid, "CapEff"))
	}
	_, st, _ := e.call("GET", "/v1/sessions/ps2r", nil, testSecret)
	sb, _ := jsonIndent(st["stats"])
	t.Logf("stats: %s", sb)
	stats, _ := st["stats"].(map[string]any)
	if ap, _ := stats["audioPackets"].(float64); ap < 50 {
		t.Errorf("audio packets %v: the core is not running frames", stats["audioPackets"])
	}
	if hm, _ := stats["hostMs"].(string); hm != "" {
		t.Logf("host stages: %s", hm)
	}
	if real {
		if len(hashes) < 3 {
			t.Errorf("the picture never changed (%d distinct frames): the game is stuck or never started", len(hashes))
		}
		b, _ := d.grab()
		d.savePNG(t, filepath.Join(out, "ps2-final.png"), b)
		t.Logf("distinct decoded frames (250 ms sampling, %d s): %d", secs, len(hashes))
	} else {
		t.Logf("synthetic BIOS: the core runs its loop, draws nothing (framesIn=%v): that is the expected result in this mode", stats["framesIn"])
	}
	rwDir := ss.rwDir
	cardBefore, _ := os.ReadFile(filepath.Join(rwDir, "pcsx2", "memcards", "Mcd001.ps2"))
	code, m, raw = e.call("DELETE", "/v1/sessions/ps2r", nil, testSecret)
	if code != 200 || m["saved"] != true {
		t.Fatalf("delete: %d %s", code, raw)
	}
	cards := ps2Cards(t, e.totv.sram)
	c1, c2 := cards[ps2SavePrefix+"Mcd001.ps2"], cards[ps2SavePrefix+"Mcd002.ps2"]
	t.Logf("save package: %d bytes zipped, cards %d and %d bytes", len(e.totv.sram), len(c1), len(c2))
	if len(cards) != 2 || len(c1) != 8650752 || len(c2) != 8650752 {
		t.Fatalf("package: %d entries, card sizes %d %d", len(cards), len(c1), len(c2))
	}
	if !real && !bytes.Equal(c1[:4096], bytes.Repeat([]byte{0xff}, 4096)) {
		t.Errorf("Mcd001.ps2 was expected to be the blank (erased, 0xFF) card the core creates; it starts %x", c1[:16]) // a game/BIOS formats it, this mode runs neither
	}
	if real && bytes.HasPrefix(c1, []byte("Sony PS2 Memory Card Format ")) {
		t.Logf("the card is formatted (the game or the BIOS used it)")
	}
	if len(cardBefore) > 0 && !bytes.Equal(cardBefore, c1) {
		t.Logf("note: the card changed between the last read and the zip (the core flushed at exit)")
	}
	// a second session restores the same cards (the core must find them formatted: it does not reformat)
	if code, _, raw := e.call("POST", "/v1/sessions", ps2RealBody(e, "ps2s"), testSecret); code != 201 {
		t.Fatalf("second session: %d %s", code, raw)
	}
	ss2 := e.srv.sessions["ps2s"]
	if b, err := os.ReadFile(filepath.Join(ss2.rwDir, "pcsx2", "memcards", "Mcd001.ps2")); err != nil || !bytes.Equal(b[:1<<20], c1[:1<<20]) {
		t.Errorf("the card was not restored (%v)", err)
	}
	e.call("DELETE", "/v1/sessions/ps2s", nil, testSecret)
}

func ps2Schedule() []uint32 {
	var seq []uint32
	for _, n := range strings.Split(envOr("PS2_DRIVE_BUTTONS", "start,cross"), ",") {
		if b, ok := pspButtonBits[strings.TrimSpace(n)]; ok {
			seq = append(seq, b)
		}
	}
	return seq
}

// TestRealPS2NoBIOSAndPS1BIOS: the real core, the real component, the error paths (PS2=1, a GPU and cores/pcsx2_libretro.so, no BIOS and no image needed):
// no BIOS directory -> bios_missing; a directory with only a PS1 BIOS (PS2_PS1_BIOS=<file>: the console's own dump, read here and copied into a temporary
// directory, never into the repository) -> bios_invalid. And what the CORE itself says when it is handed that PS1 BIOS (the component's own check
// is what refuses it first; this proves the core would have refused it too).
func TestRealPS2NoBIOSAndPS1BIOS(t *testing.T) {
	e, _, _ := ps2RealEnv(t)
	e.srv.cfg.PS2BiosDir = ""
	if code, _, raw := e.call("POST", "/v1/sessions", ps2RealBody(e, "nb"), testSecret); code != 502 || !strings.Contains(string(raw), "bios_missing") {
		t.Fatalf("no BIOS: %d %s", code, raw)
	}
	_, m, raw := e.call("GET", "/v1/health", nil, testSecret)
	if ps2, _ := m["ps2"].(map[string]any); ps2 == nil || ps2["bios"] != false || ps2["reason"] != "bios_missing" {
		t.Fatalf("health: %s", raw)
	}
	ps1 := os.Getenv("PS2_PS1_BIOS")
	if ps1 == "" {
		t.Skip("PS2_PS1_BIOS=<a PS1 BIOS dump> not set: the invalid-BIOS half is skipped")
	}
	b, err := os.ReadFile(ps1)
	if err != nil {
		t.Skipf("PS2_PS1_BIOS unreadable: %v", err)
	}
	e.srv.cfg.PS2BiosDir = writeBIOSDir(t, map[string][]byte{"ps1.bin": b})
	if code, _, raw := e.call("POST", "/v1/sessions", ps2RealBody(e, "ps1"), testSecret); code != 502 || !strings.Contains(string(raw), "bios_invalid") {
		t.Fatalf("PS1 BIOS: %d %s", code, raw)
	}
	_, m, _ = e.call("GET", "/v1/health", nil, testSecret)
	if ps2, _ := m["ps2"].(map[string]any); ps2["bios"] != false || ps2["reason"] != "bios_invalid" {
		t.Fatalf("health with a PS1 BIOS: %v", ps2)
	}
	if ents, _ := os.ReadDir(e.srv.cfg.BigTmp); len(ents) != 0 {
		t.Fatalf("refused sessions left %d entries", len(ents))
	}
}
