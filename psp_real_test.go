// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 victobriel

package main

import (
	"crypto/md5"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
)

// ---- real PSP (PSP=1): PPSSPP libretro on the GPU, a homebrew EBOOT.PBP/ISO from OUTSIDE the repo, served as a file by the fake Totv ----
//
// Env: PSP_ROM (required, no default: there is no commercial game here), PSP_SYSTEM (dir that holds PPSSPP/, default cores/system),
// PSP_SECS (30), PSP_OUT (PNG dir), PSP_CORE_OPTS ("ppsspp_x=y;...").

func pspRealEnv(t *testing.T) (*testEnv, string) {
	t.Helper()
	if os.Getenv("PSP") != "1" {
		t.Skip("set PSP=1 and PSP_ROM=<file> (make psp)")
	}
	rom := os.Getenv("PSP_ROM")
	if _, err := os.Stat(rom); rom == "" || err != nil {
		t.Skip("PSP_ROM not set or not readable: no ROM, no real test")
	}
	cwd, _ := os.Getwd()
	coresDir := filepath.Join(cwd, "cores")
	if _, err := os.Stat(filepath.Join(coresDir, "ppsspp_libretro.so")); err != nil {
		t.Skip("cores/ppsspp_libretro.so not built")
	}
	sysDir := os.Getenv("PSP_SYSTEM")
	if sysDir == "" {
		sysDir = filepath.Join(coresDir, "system")
	}
	rw := t.TempDir()
	e := newEnv(t, func(c *config) {
		c.CoresDir, c.DevTestCore, c.PSPSystemDir, c.RWTmp, c.DirSaveEvery = coresDir, false, sysDir, rw, 5*time.Second
		c.RWAllowDisk = true
		c.PSPCoreOpts = os.Getenv("PSP_CORE_OPTS")
	})
	skipWithoutGPU(t, e)
	if !e.srv.assetsOK() {
		t.Skipf("PPSSPP assets missing in %s/PPSSPP (scripts/psp-assets.sh)", sysDir)
	}
	e.totv.romPath = rom
	return e, rom
}

// pspGameMode: PSP_KIND=game means a commercial (or any non-homebrew) ROM: the cube/white-background premises of scripts/psp-homebrew do
// not hold (no stick-driven cube, no RUNS.BIN counter), so the image assertions become "the picture really moves": distinct decoded frames
// and a changed final screen, with the input schedule of PSP_DRIVE_* (see TestRealPSPDrive). Default (unset) = the homebrew premises.
func pspGameMode() bool { return os.Getenv("PSP_KIND") == "game" }

var pspButtonBits = map[string]uint32{"cross": 1 << 4, "circle": 1 << 5, "square": 1 << 6, "triangle": 1 << 7, "l": 1 << 8, "r": 1 << 9,
	"select": 1 << 15, "start": 1 << 14, "up": 1 << 0, "down": 1 << 1, "left": 1 << 2, "right": 1 << 3}

func pspEnvInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil && v > 0 {
		return v
	}
	return def
}

// pspSchedule: the buttons of PSP_DRIVE_BUTTONS (comma list), pressed one after the other every PSP_DRIVE_EVERY s (held PSP_DRIVE_HOLD_MS) from PSP_DRIVE_START
// for PSP_DRIVE_PRESS_SECS, then no input.
func pspSchedule(t *testing.T) (seq []uint32, start, press, every int) {
	t.Helper()
	for _, n := range strings.Split(envOr("PSP_DRIVE_BUTTONS", "cross"), ",") {
		b, ok := pspButtonBits[strings.TrimSpace(n)]
		if !ok {
			t.Fatalf("unknown button %q", n)
		}
		seq = append(seq, b)
	}
	return seq, pspEnvInt("PSP_DRIVE_START", 0), pspEnvInt("PSP_DRIVE_PRESS_SECS", 20), pspEnvInt("PSP_DRIVE_EVERY", 2)
}

func pspBody(e *testEnv, id string) map[string]any {
	b := e.createBody(id)
	b["platform"], b["core"] = "psp", "server_ppsspp"
	return b
}

// TestRealPSP: 30 s of a real PPSSPP session, same measurements as TestRealN64 (fps, stages, CPU, RSS, GPU), PNGs, and the save package
// at the end. The homebrew of scripts/psp-homebrew writes ms0:/PSP/SAVEDATA/TOTV00000/RUNS.BIN (a counter): it must come back in the zip.
func TestRealPSP(t *testing.T) {
	e, romPath := pspRealEnv(t)
	secs := 30
	if v, err := strconv.Atoi(os.Getenv("PSP_SECS")); err == nil && v > 0 {
		secs = v
	}
	out := os.Getenv("PSP_OUT")
	if out == "" {
		out = t.TempDir()
	}
	os.MkdirAll(out, 0o755)
	fi, _ := os.Stat(romPath)
	t0 := time.Now()
	if code, m, _ := e.call("POST", "/v1/sessions", pspBody(e, "psp"), testSecret); code != 201 {
		t.Fatalf("create: %d %v", code, m)
	}
	t.Logf("RESULT session start (download %d bytes + core load + first frame to hello): %.2f s", fi.Size(), time.Since(t0).Seconds())
	p := newTestPeer(t)
	defer p.pc.Close()
	w, h := e.srv.size(coreTable["ppsspp"])
	d := startDecoder(t, p, w, h)
	p.negotiate(t, e, "psp")
	waitFor(t, "decoded video", 20*time.Second, func() bool { _, n := d.grab(); return n > 5 })
	e.srv.mu.Lock()
	ss := e.srv.sessions["psp"]
	e.srv.mu.Unlock()
	pid := ss.corePID()
	t.Logf("sandbox flags %d (1 seccomp, 2 landlock, 4 landlock net, 8 netns, 16 gpu); child pid %d threads %s; /proc status Seccomp=%s NoNewPrivs=%s CapEff=%s",
		ss.flags, pid, procField(pid, "Threads"), procField(pid, "Seccomp"), procField(pid, "NoNewPrivs"), procField(pid, "CapEff"))
	if ss.flags&3 != 3 {
		t.Fatalf("sandbox flags %d", ss.flags)
	}
	time.Sleep(2 * time.Second) // let the core start everything it is going to start
	tasks, _ := filepath.Glob(fmt.Sprintf("/proc/%d/task/*/status", pid))
	bad := 0
	for _, f := range tasks {
		b, _ := os.ReadFile(f)
		if checkProcStatus(string(b)) != nil {
			bad++
		}
	}
	t.Logf("RESULT threads after 2 s of play: %d, without Seccomp:2/NoNewPrivs:1: %d", len(tasks), bad)
	if bad != 0 || len(tasks) == 0 {
		t.Errorf("%d of %d threads are not under the filter", bad, len(tasks))
	}
	if fds, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid)); err == nil {
		var l []string
		for _, f := range fds {
			if tgt, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, f.Name())); err == nil && !strings.HasPrefix(tgt, "pipe:") {
				if strings.HasPrefix(tgt, "socket:") {
					t.Errorf("the child still holds a socket (%s): the driver's D-Bus connection must be closed before the sandbox", tgt)
				}
				l = append(l, tgt)
			}
		}
		sort.Strings(l)
		t.Logf("child fds: %v", l)
	}
	// the analog stick: the homebrew moves the cube with the left stick X; centroid of the non-background pixels (clear color 0x334455)
	centroid := func(b []byte) float64 {
		var sx, n float64
		for i := 0; i+2 < len(b); i += 3 {
			if !near([3]int{int(b[i]), int(b[i+1]), int(b[i+2])}, 0x33, 0x44, 0x55, 14) {
				sx += float64((i / 3) % d.w)
				n++
			}
		}
		if n == 0 {
			return -1
		}
		return sx / n
	}
	stick := func(x int8) float64 {
		var acc, k float64
		for i := 0; i < 20; i++ {
			p.sendInputAxes(0, [4]int8{x, 0, 0, 0})
			time.Sleep(80 * time.Millisecond)
			if i >= 10 {
				b, _ := d.grab()
				acc += centroid(b)
				k++
			}
		}
		return acc / k
	}
	game := pspGameMode()
	if !game { // homebrew only: the cube follows the left stick
		right, left := stick(120), stick(-120)
		p.sendInputAxes(0, [4]int8{})
		t.Logf("RESULT analog stick (left X axis of the packet -> PSP analog): picture centroid x = %.0f with +120, %.0f with -120 (of %d)", right, left, d.w)
		if right < left+40 {
			t.Errorf("the left stick did not move the picture (%.0f vs %.0f)", right, left)
		}
		time.Sleep(2 * time.Second)
	}
	seq, startS, pressS, everyS := pspSchedule(t)
	first, _ := d.grab()
	var mu sync.Mutex
	hashes := map[[16]byte]bool{}
	stopHash := make(chan struct{})
	hashDone := make(chan struct{})
	go func() { // distinct decoded frames, sampled every 250 ms: a stuck emulator (core repeating its last frame) shows as 1
		defer close(hashDone)
		for {
			select {
			case <-stopHash:
				return
			case <-time.After(250 * time.Millisecond):
				b, _ := d.grab()
				hs := md5.Sum(b)
				mu.Lock()
				hashes[hs] = true
				mu.Unlock()
			}
		}
	}()
	type snap struct{ fIn, fEnc, aud float64 }
	get := func() snap {
		_, st, _ := e.call("GET", "/v1/sessions/psp", nil, testSecret)
		s := st["stats"].(map[string]any)
		return snap{s["framesIn"].(float64), s["framesEncoded"].(float64), s["audioPackets"].(float64)}
	}
	s0, c0, w0, pc0 := get(), readProcCPU(pid), time.Now(), readProcCPU(os.Getpid())
	var gpuUtil, gpuMem, rss, perSec []float64
	var maxLum float64
	pi := 0
	prev := s0
	shot := map[int]bool{3: true, 8: true, 15: true, 25: true}
	for i := 1; i <= secs; i++ {
		time.Sleep(time.Second)
		if !game {
			if i%3 == 0 {
				p.sendInput(1 << 4) // a (south) = Cross
				time.Sleep(120 * time.Millisecond)
				p.sendInput(0)
			}
			p.sendInputAxes(0, [4]int8{int8(90 * ((i%2)*2 - 1)), 0, 0, 0})
		} else if i >= startS && i < startS+pressS && (i-startS)%everyS == 0 {
			p.sendInput(seq[pi%len(seq)])
			pi++
			time.Sleep(time.Duration(pspEnvInt("PSP_DRIVE_HOLD_MS", 150)) * time.Millisecond)
			p.sendInput(0)
		}
		cur := get()
		perSec = append(perSec, cur.fIn-prev.fIn)
		prev = cur
		u, m, _ := gpuSample(pid)
		gpuUtil, gpuMem = append(gpuUtil, u), append(gpuMem, m)
		kb, _ := strconv.ParseFloat(strings.Fields(procField(pid, "VmRSS") + " 0")[0], 64)
		rss = append(rss, kb/1024)
		if i%2 == 0 {
			b, _ := d.grab()
			var l float64
			for k := 0; k < len(b); k += 30 {
				l += float64(b[k])
			}
			if l /= float64(len(b) / 30); l > maxLum {
				maxLum = l
			}
		}
		if shot[i] || (game && i%5 == 0) {
			b, _ := d.grab()
			d.savePNG(t, filepath.Join(out, fmt.Sprintf("psp-t%02d.png", i)), b)
			t.Logf("BBOX t=%d %v", i, contentBBox(b, d.w, d.h, 24))
		}
	}
	s1, c1, w1, pc1 := get(), readProcCPU(pid), time.Now(), readProcCPU(os.Getpid())
	el := w1.Sub(w0).Seconds()
	q := func(v []float64, f float64) float64 { return percentile(append([]float64(nil), v...), f) }
	t.Logf("RESULT frames from core in %.1fs: %.0f = %.2f fps sustained; encoded %.0f (%.2f fps); audio RTP packets %.0f (%.1f/s, 50 expected)", el, s1.fIn-s0.fIn, (s1.fIn-s0.fIn)/el, s1.fEnc-s0.fEnc, (s1.fEnc-s0.fEnc)/el, s1.aud-s0.aud, (s1.aud-s0.aud)/el)
	t.Logf("RESULT frames per second (1 s buckets): min %.0f p50 %.0f max %.0f", q(perSec, 0), q(perSec, .5), q(perSec, 1))
	t.Logf("RESULT retro-emu child CPU %.0f%% of one core (all threads); component+test client process %.0f%% of one core", (c1-c0)/el*100, (pc1-pc0)/el*100)
	t.Logf("RESULT child RSS %.0f..%.0f MB; whole-GPU util p50 %.0f%% max %.0f%%; GPU mem used %.0f MB; threads now %s", q(rss, 0), q(rss, 1), q(gpuUtil, .5), q(gpuUtil, 1), q(gpuMem, .5), procField(pid, "Threads"))
	if hm := ss.hostMsV.Load(); hm != nil {
		t.Logf("RESULT host stages (last 120-frame window, avg): %s", hm)
	}
	t.Logf("RESULT x264 encode %+v | frame->wire %+v | input->frame %+v | dropped %d", ss.encodeMs.pct(), ss.wireMs.pct(), ss.inputToFrame.pct(), ss.vDropped.Load())
	if got := (s1.fIn - s0.fIn) / el; got < 50 {
		t.Errorf("sustained %.1f fps < 50", got)
	}
	b, _ := d.grab()
	d.savePNG(t, filepath.Join(out, "psp-frame.png"), b)
	minLum := 20.0
	if game {
		minLum = 3 // commercial games fade through dark screens; "moves" is asserted below
	}
	if maxLum < minLum {
		t.Errorf("picture stayed black (max mean luma %.1f)", maxLum)
	}
	close(stopHash)
	<-hashDone
	mu.Lock()
	nd := len(hashes)
	mu.Unlock()
	t.Logf("RESULT distinct decoded frames (250 ms sampling, %d s): %d; final screen equals the one at the start of the window: %v", secs, nd, string(b) == string(first))
	if game && (nd < 3 || string(b) == string(first)) {
		t.Errorf("game picture did not move (distinct frames %d, final == first %v): stuck, or the input schedule does not advance the game", nd, string(b) == string(first))
	}
	// the save package, created by the parent after the child has exited
	rwBefore := ss.rwDir
	var rwFiles []string
	filepath.Walk(rwBefore, func(p string, i os.FileInfo, err error) error {
		if err == nil && !i.IsDir() {
			rel, _ := filepath.Rel(rwBefore, p)
			rwFiles = append(rwFiles, fmt.Sprintf("%s(%d)", rel, i.Size()))
		}
		return nil
	})
	t.Logf("RESULT files the core wrote in its tmpfs dir while running: %v", rwFiles)
	code, m, raw := e.call("DELETE", "/v1/sessions/psp", nil, testSecret)
	if code != 200 {
		t.Fatalf("delete: %d %s", code, raw)
	}
	t.Logf("RESULT delete -> %v", m)
	if _, err := os.Stat(rwBefore); err == nil {
		t.Errorf("writable dir %s still exists after the session", rwBefore)
	}
	e.totv.mu.Lock()
	z := append([]byte(nil), e.totv.sram...)
	puts := e.totv.puts
	e.totv.mu.Unlock()
	if len(z) == 0 {
		t.Logf("RESULT save: no save package was uploaded (the game wrote nothing under PSP/SAVEDATA)")
		return
	}
	if game {
		t.Logf("RESULT save package of the game: %d bytes zip, %d PUTs (no RUNS.BIN premise for a commercial game)", len(z), puts)
		return
	}
	got, err := savedCounter(t, z)
	t.Logf("RESULT save package: %d bytes zip, %d PUTs, entries %v err %v", len(z), puts, got, err)
	if err != nil {
		t.Errorf("package invalid: %v", err)
	}
}

// TestRealPSPLatency: input to decoded picture, like TestLatencyProbeGL. The homebrew turns the whole background white while Cross is held;
// the threshold is relative to the measured baseline because the cube is on screen.
func TestRealPSPLatency(t *testing.T) {
	e, _ := pspRealEnv(t)
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if code, m, _ := e.call("POST", "/v1/sessions", pspBody(e, "pspl"), testSecret); code != 201 {
		t.Fatalf("create: %d %v", code, m)
	}
	p := newTestPeer(t)
	defer p.pc.Close()
	ff := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-fflags", "nobuffer", "-flags", "low_delay", "-probesize", "32",
		"-analyzeduration", "0", "-threads", "1", "-f", "h264", "-i", "pipe:0", "-vf", "scale=16:12:flags=neighbor,format=gray", "-f", "rawvideo", "-fps_mode", "passthrough", "pipe:1")
	in, _ := ff.StdinPipe()
	out, _ := ff.StdoutPipe()
	if err := ff.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { in.Close(); ff.Process.Kill(); ff.Wait() }()
	var mu sync.Mutex
	var mean float64
	var stamp time.Time
	go func() {
		buf := make([]byte, 192)
		for {
			if _, err := io.ReadFull(out, buf); err != nil {
				return
			}
			s := 0
			for _, b := range buf {
				s += int(b)
			}
			mu.Lock()
			mean, stamp = float64(s)/192, time.Now()
			mu.Unlock()
		}
	}()
	var dep codecs.H264Packet
	var au []byte
	p.onVideo = func(pkt *rtp.Packet) {
		if b, err := dep.Unmarshal(pkt.Payload); err == nil {
			au = append(au, b...)
		}
		if pkt.Marker && len(au) > 0 {
			in.Write(au)
			au = au[:0]
		}
	}
	p.negotiate(t, e, "pspl")
	cur := func() (float64, time.Time) { mu.Lock(); defer mu.Unlock(); return mean, stamp }
	wait := func(cond func(float64) bool, d time.Duration) bool {
		end := time.Now().Add(d)
		for time.Now().Before(end) {
			if m, s := cur(); !s.IsZero() && cond(m) {
				return true
			}
			time.Sleep(500 * time.Microsecond)
		}
		return false
	}
	if !wait(func(m float64) bool { return m > 5 }, 15*time.Second) {
		t.Fatal("no decoded video")
	}
	if pspGameMode() {
		// A commercial game has no white-on-Cross premise. Probe: at the title of the game (wait PSP_LAT_WAIT s, default 24, for the intros)
		// PSP_LAT_PRESS (default cross) opens a dialog that dims the picture, PSP_LAT_UNDO (default circle) closes it: the "press" time is
		// input -> decoded frame whose mean luma fell below 60% of the baseline, "release" -> back above 90%. The game's own fade is part of
		// the number (an upper bound of the pipeline's latency).
		time.Sleep(time.Duration(pspEnvInt("PSP_LAT_WAIT", 24)) * time.Second)
		base, _ := cur()
		pb, ub := pspButtonBits[envOr("PSP_LAT_PRESS", "cross")], pspButtonBits[envOr("PSP_LAT_UNDO", "circle")]
		t.Logf("game latency probe: baseline mean luma %.1f, dim < %.1f, restored > %.1f", base, base*.6, base*.9)
		var press, release []float64
		for i := 0; i < 8; i++ {
			t0 := p.sendInput(pb)
			if wait(func(m float64) bool { return m < base*.6 }, 3*time.Second) {
				press = append(press, float64(time.Since(t0).Microseconds())/1000)
			}
			p.sendInput(0)
			time.Sleep(1500 * time.Millisecond)
			t1 := p.sendInput(ub)
			if wait(func(m float64) bool { return m > base*.9 }, 3*time.Second) {
				release = append(release, float64(time.Since(t1).Microseconds())/1000)
			}
			p.sendInput(0)
			time.Sleep(1500 * time.Millisecond)
		}
		t.Logf("PROBE [PPSSPP game, GPU] input->decoded frame (game fade included): press n=%d p50=%.1f ms p95=%.1f ms | release n=%d p50=%.1f ms p95=%.1f ms",
			len(press), percentile(press, .5), percentile(press, .95), len(release), percentile(release, .5), percentile(release, .95))
		if len(press) < 4 || len(release) < 4 {
			t.Errorf("only %d presses / %d releases seen of 8", len(press), len(release))
		}
		return
	}
	time.Sleep(3 * time.Second) // let the homebrew start drawing
	base, _ := cur()
	hi := 215.0 // the white background pushes the mean of the 16x12 grey picture well above any frame with the dark one
	t.Logf("baseline mean luma %.1f, press threshold %.0f", base, hi)
	var press, release []float64
	for i := 0; i < 30; i++ {
		wait(func(m float64) bool { return m < hi-40 }, 2*time.Second)
		time.Sleep(400 * time.Millisecond)
		t0 := p.sendInput(1 << 4)
		if wait(func(m float64) bool { return m > hi }, 2*time.Second) {
			press = append(press, float64(time.Since(t0).Microseconds())/1000)
		}
		time.Sleep(300 * time.Millisecond)
		t1 := p.sendInput(0)
		if wait(func(m float64) bool { return m < hi-40 }, 2*time.Second) {
			release = append(release, float64(time.Since(t1).Microseconds())/1000)
		}
	}
	q := func(v []float64, f float64) float64 { return percentile(v, f) }
	t.Logf("PROBE [PPSSPP, GPU] input->decoded frame (loopback, x264 ultrafast, ffmpeg sw decode): press n=%d p50=%.1f ms p95=%.1f ms max=%.1f | release n=%d p50=%.1f ms p95=%.1f ms",
		len(press), q(press, .5), q(press, .95), q(press, 1), len(release), q(release, .5), q(release, .95))
	_, m, _ := e.call("GET", "/v1/sessions/pspl", nil, testSecret)
	b, _ := jsonIndent(m["stats"])
	t.Logf("PROBE server stats: %s", b)
	if len(press) < 10 {
		t.Errorf("only %d of 30 presses seen", len(press))
	}
}

// TestRealPSPDrive: a COMMERCIAL (or any) game driven by a button schedule, to see whether the picture really moves. Unlike TestRealPSP it
// makes no assumption about the picture (no cube, no white background): the evidence is the set of DISTINCT decoded frames (hash sampled
// every 250 ms), PNGs every 5 s, and the core's own frame counter. Env: PSP_ROM (required), PSP_DRIVE_BUTTONS (comma list of
// cross,circle,square,triangle,start,select,up,down,left,right,l,r; default cross), PSP_DRIVE_START (first press, s, default 0), PSP_DRIVE_PRESS_SECS (input phase length, default 20),
// PSP_DRIVE_EVERY (seconds between presses, default 2), PSP_DRIVE_HOLD_MS (how long each press is held, default 150), PSP_SECS (total, default 60), PSP_SHOT_EVERY (PNG period in s, default 5), PSP_OUT (PNG dir).
func TestRealPSPDrive(t *testing.T) {
	e, _ := pspRealEnv(t)
	seq, startS, pressS, everyS := pspSchedule(t)
	atoi := pspEnvInt
	total, pressFor, every := atoi("PSP_SECS", 60), pressS, everyS
	out := os.Getenv("PSP_OUT")
	if out == "" {
		out = t.TempDir()
	}
	os.MkdirAll(out, 0o755)
	if code, m, _ := e.call("POST", "/v1/sessions", pspBody(e, "pspd"), testSecret); code != 201 {
		t.Fatalf("create: %d %v", code, m)
	}
	p := newTestPeer(t)
	defer p.pc.Close()
	w, h := e.srv.size(coreTable["ppsspp"])
	d := startDecoder(t, p, w, h)
	p.negotiate(t, e, "pspd")
	waitFor(t, "decoded video", 20*time.Second, func() bool { _, n := d.grab(); return n > 5 })
	e.srv.mu.Lock()
	ss := e.srv.sessions["pspd"]
	e.srv.mu.Unlock()
	pid := ss.corePID()
	hashes := map[[16]byte]int{}
	var order []string
	var lastH [16]byte
	t0 := time.Now()
	startAt := time.Duration(startS) * time.Second
	nextPress, pi := startAt, 0
	shotEvery := time.Duration(atoi("PSP_SHOT_EVERY", 5)) * time.Second
	nextShot := shotEvery
	for el := time.Duration(0); el < time.Duration(total)*time.Second; el = time.Since(t0) {
		if el < startAt+time.Duration(pressFor)*time.Second && el >= nextPress {
			b := seq[pi%len(seq)]
			pi++
			p.sendInput(b)
			time.Sleep(time.Duration(atoi("PSP_DRIVE_HOLD_MS", 150)) * time.Millisecond)
			p.sendInput(0)
			nextPress = el + time.Duration(every)*time.Second
		}
		b, _ := d.grab()
		hs := md5.Sum(b)
		if hs != lastH {
			hashes[hs]++
			lastH = hs
			order = append(order, fmt.Sprintf("%.1f", el.Seconds()))
		}
		if el >= nextShot {
			d.savePNG(t, filepath.Join(out, fmt.Sprintf("drive-t%03d.png", int(el.Seconds()))), b)
			st, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
			_, st2, _ := e.call("GET", "/v1/sessions/pspd", nil, testSecret)
			var fin float64
			if s, ok := st2["stats"].(map[string]any); ok {
				fin, _ = s["framesIn"].(float64)
			}
			f := strings.Fields(string(st))
			t.Logf("t=%3.0fs distinct-frames=%d changes=%d framesIn=%.0f child state=%s utime+stime=%s+%s", el.Seconds(), len(hashes), len(order), fin, f[2], f[13], f[14])
			nextShot += shotEvery
		}
		time.Sleep(250 * time.Millisecond)
	}
	d.savePNG(t, filepath.Join(out, "drive-final.png"), func() []byte { b, _ := d.grab(); return b }())
	t.Logf("RESULT distinct frames seen (250 ms sampling, %d s): %d, picture changes %d", total, len(hashes), len(order))
	if len(hashes) < 3 {
		t.Errorf("the picture never changed (distinct frames %d): the game is stuck or the input does not reach it", len(hashes))
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
